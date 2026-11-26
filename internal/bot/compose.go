package bot

// Alerts written by the LLM. It reads the facts, the raw lines, what else is going on and what
// operators taught noc-bot, then writes what an operator would post: a title and a short note in
// its own words, as long as the situation needs. Code still owns what must not be wrong:
// every number, address and time in the text must appear in what the LLM was given (or found
// with its tools), or the text is thrown away and the plain fact list is posted instead. Mentions,
// team and severity are added by code. Raw logs go to the thread, folded.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/IWNOC/noc-bot/internal/incident"
	"github.com/IWNOC/noc-bot/internal/llm"
)

// LogPoster can fold raw logs into a thread.
type LogPoster interface {
	PostLogs(ctx context.Context, thread, title, logs string) error
}

const composeSystem = `あなたはイベントネットワークの NOC の運用担当です。検知した出来事を Slack のチャンネルに知らせる投稿を書きます。
- 読む人は運用者です。何が起きているか、気にすべきか、次に何を見ればよいかが一目でわかるように。
- 長さは状況に合わせる：よくある軽いものは1〜2行、影響が大きそうなものは影響・見立て・次の一手まで。
- 事実と推測を分ける。推測には「可能性」「要確認」を付ける。
- 数字・IP アドレス・時刻を書くときは、与えられた情報か道具の結果にあるものを正確に写す（書かなくてもよい）。
- 生のログは貼らない（スレッドに別に添付される）。
- 運用者が教えてくれたこと（記憶）があれば踏まえる。
出来事の種類の意味（言い換えて強くしないこと）:
- rate: あるログのパターンが普段よりずっと多く出ている
- silence: ある送信元から**ログが届かなくなった**。機器やサービスが止まったとは限らない（ログ設定・経路・送信元の都合もありうる）
- clock_skew: 送信元が書いた時刻が、受信した時刻とずれている（その機器の時計か設定の問題。NTP サーバーの停止とは限らない）
- odd: 初めて見る、またはいつもと違うと判断されたログ
- window: 全体を見て、いつもと違うところがありそう
JSON だけで答える：{"title": "<一行の見出し。絵文字なし>", "body": "<本文。Slack の mrkdwn>"}`

func (b *Bot) compose(ctx context.Context, in incident.Incident) (title, body, note string, ok bool) {
	lm := b.agentLLM()
	if lm == nil {
		return "", "", "", false
	}
	facts, _ := json.MarshalIndent(in.Facts, "", "  ")
	b.mu.Lock()
	ex := strings.Join(b.examples[in.TplID], "\n")
	b.mu.Unlock()
	var sb strings.Builder
	fmt.Fprintf(&sb, "出来事の種類: %s\n送信元: %s\n", in.Kind, in.Host)
	if in.Template != "" {
		fmt.Fprintf(&sb, "ログのパターン: %s\n", in.Template)
	}
	fmt.Fprintf(&sb, "事実:\n%s\n", facts)
	if ex != "" {
		fmt.Fprintf(&sb, "直近の生ログ:\n%s\n", ex)
	}
	if rel := b.Incidents.Related(in.Key, b.Now()); len(rel) > 0 {
		fmt.Fprintf(&sb, "同時に起きていること: %s\n", strings.Join(rel, "; "))
	}
	for _, m := range b.memoryAbout(ctx, in.Host) {
		fmt.Fprintf(&sb, "運用者が教えてくれたこと: %s\n", m)
	}
	input := sb.String()
	msgs := []llm.Message{{Role: "system", Content: composeSystem}, {Role: "user", Content: input}}
	out, steps, err := lm.Run(ctx, msgs, b.Tools(), 3, 1500, true)
	if err != nil {
		b.Log.Warn("compose", "incident", in.ID, "err", err)
		return "", "", "", false
	}
	var got struct{ Title, Body string }
	if json.Unmarshal([]byte(jsonPart(out)), &got) != nil || strings.TrimSpace(got.Title) == "" {
		// an empty or cut reply: ask once more, without tools, for the JSON only
		if m, err := lm.Chat(ctx, msgs, nil, 1500, true); err == nil {
			out = m.Content
			json.Unmarshal([]byte(jsonPart(out)), &got)
		}
	}
	if strings.TrimSpace(got.Title) == "" {
		b.Log.Warn("compose: unparsable", "out", clipText(out, 200))
		return "", "", "", false
	}
	corpus := input
	for _, s := range steps {
		corpus += "\n" + s.Result
	}
	if bad := ungrounded(got.Title+"\n"+got.Body, corpus); len(bad) > 0 {
		b.Log.Warn("compose: dropped, values not in the evidence", "incident", in.ID, "values", bad)
		return "", "", "", false
	}
	return strings.TrimSpace(got.Title), strings.TrimSpace(got.Body), got.Title + " " + got.Body, true
}

var valueRe = regexp.MustCompile(`(?i)` +
	`(?:[0-9a-f]{1,4}:){2,7}[0-9a-f]{0,4}(?:/\d+)?` + // IPv6 (and MACs)
	`|\d{1,3}(?:\.\d{1,3}){3}(?:/\d+)?` + // IPv4
	`|\d{1,2}:\d{2}(?::\d{2})?` + // clock times
	`|\d+(?:\.\d+)?`) // other numbers

// ungrounded lists the numbers, addresses and times in text that do not appear in the evidence.
// Small numbers that are part of ordinary prose (1, 2, 3/3 severities, list numbering) are allowed.
func ungrounded(text, evidence string) []string {
	ev := strings.ToLower(evidence)
	seen := map[string]bool{}
	var bad []string
	for _, v := range valueRe.FindAllString(text, -1) {
		lv := strings.ToLower(v)
		if seen[lv] {
			continue
		}
		seen[lv] = true
		if len(lv) == 1 && lv >= "0" && lv <= "9" {
			continue // "1件", "3/3", numbering
		}
		if !strings.Contains(ev, lv) {
			bad = append(bad, v)
		}
	}
	sort.Strings(bad)
	return bad
}

// renderComposed puts code-owned parts (icon, mention, team, severity) around the LLM's text.
func renderComposed(ev incident.Event, title, body, mention string) string {
	in := ev.Incident
	var sb strings.Builder
	sb.WriteString("🔴 *" + title + "*")
	if in.Page && mention != "" {
		sb.WriteString(" " + mention)
	}
	sb.WriteString("\n" + body + "\n")
	var meta []string
	if in.Team != "" {
		meta = append(meta, "担当："+in.Team)
	}
	if in.Severity >= 0 {
		meta = append(meta, fmt.Sprintf("重要度：%d/3", in.Severity))
	}
	meta = append(meta, "送信元："+in.Host, "ID："+in.ID)
	if in.Why != "" {
		meta = append(meta, "すぐ知らせた理由："+in.Why)
	}
	sb.WriteString("_" + strings.Join(meta, "　") + "_")
	return sb.String()
}
