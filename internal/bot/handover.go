package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/IWNOC/noc-bot/internal/incident"
	"github.com/IWNOC/noc-bot/internal/laya"
	"github.com/IWNOC/noc-bot/internal/llm"
)

// Handover notes: at shift changes (09:00 and 18:00 JST) noc-bot writes what the next people need
// to know, the way an operator leaves a note. They are stored and searchable (notebook_search).
var handoverHours = map[int]bool{9: true, 18: true}

const handoverSystem = `あなたはイベントネットワークの NOC の運用担当です。次の担当者への「引き継ぎメモ」を書きます。
- 5〜7行。最初の1行で全体の様子（落ち着いていたか、荒れたか）。
- 次に「続いていること」「終わったこと」「次の人に見てほしいこと」を、あるものだけ。
- ふつうの言葉で。内部の用語（パターンの ID、NORMAL など）、アカウント名、MAC アドレスは書かない。
- 数字・時刻（JST）・機器名は、与えられた情報にあるものだけを正確に。
- 書式は Slack 用（太字は *こう*、箇条書きは「• 」）。`

func (b *Bot) handover(ctx context.Context, now time.Time) {
	n := now.In(jst)
	if !handoverHours[n.Hour()] || n.Minute() != 0 || now.Sub(b.lastHand) < time.Hour || b.agentLLM() == nil {
		return
	}
	b.lastHand = now
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		from := now.Add(-15 * time.Hour)
		if n.Hour() == 18 {
			from = now.Add(-9 * time.Hour)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "期間: %s 〜 %s JST\n", from.In(jst).Format("01/02 15:04"), n.Format("01/02 15:04"))
		if bodies, err := b.Store.IncidentsBetween(ctx, from, now, 200); err == nil {
			sb.WriteString("通知したこと:\n")
			c := 0
			for _, raw := range bodies {
				var in incident.Incident
				if json.Unmarshal([]byte(raw), &in) != nil || in.Tier == TierSilent {
					continue
				}
				fmt.Fprintf(&sb, "- %s %s: %s (%s)\n", in.Opened.In(jst).Format("15:04"), in.Host, b.friendlyName(ctx, in.Title, in.TplID, in.Template, 100), in.Tier)
				if c++; c >= 40 {
					break
				}
			}
		}
		if ns, err := b.Store.NotesBetween(ctx, from, now, 40, "observation", "hypothesis", "accepted", "memory"); err == nil && len(ns) > 0 {
			sb.WriteString("調査担当のメモと、人が登録したこと:\n")
			for _, x := range ns {
				sb.WriteString("- " + clipText(x, 260) + "\n")
			}
		}
		if b.Zabbix != nil {
			if ps, err := b.Zabbix.Problems(ctx); err == nil {
				sb.WriteString("Zabbix で今も出ている障害:\n")
				for i, p := range ps {
					if p.Severity < 3 || i >= 15 {
						continue
					}
					fmt.Fprintf(&sb, "- %s: %s (%s から)\n", p.Host, p.Name, p.Since.In(jst).Format("01/02 15:04"))
				}
			}
		}
		for _, k := range b.Rules.Known(now) {
			fmt.Fprintf(&sb, "既知の問題: %s\n", k.Reason)
		}
		m, err := b.agentLLM().Chat(ctx, []llm.Message{{Role: "system", Content: handoverSystem}, {Role: "user", Content: sb.String()}}, nil, 900, false)
		text := strings.TrimSpace(m.Content)
		if err != nil || len([]rune(text)) < 20 {
			b.Log.Warn("handover", "err", err)
			return
		}
		text = forChannel(toMrkdwn(b.inJapanese(ctx, text)))
		b.Store.Note(ctx, now, "handover", n.Format("2006-01-02 15:04"), text, "noc-bot")
		if _, err := b.Notify.Post(ctx, "📋 *引き継ぎメモ*（"+n.Format("01/02 15:04")+" 時点・AI が作成）\n"+text, ""); err != nil {
			b.Log.Warn("handover post", "err", err)
		}
	}()
}

// asks picks up to two patterns the teacher could not settle (asked twice, two answers) to put
// before people in the digest. Each pattern is asked once.
func (b *Bot) asks(ctx context.Context, now time.Time) []askItem {
	notes, err := b.Store.NotesBetweenRaw(ctx, now.Add(-6*time.Hour), now, "disputed")
	if err != nil {
		return nil
	}
	var out []askItem
	for _, n := range notes {
		pid, ok := strings.CutPrefix(n.Subject, "line:")
		if !ok || pid == "" || !strings.Contains(","+n.Text+",", ",odd,") {
			continue
		}
		b.mu.Lock()
		seen := b.asked[pid]
		b.asked[pid] = true
		b.mu.Unlock()
		if seen {
			continue
		}
		meta, ok := b.Rules.Template(pid)
		if !ok {
			continue
		}
		out = append(out, askItem{ID: pid, Host: meta.Host, Name: b.friendlyName(ctx, "", pid, meta.Text, 90), Example: clipText(meta.Example, 160)})
		if len(out) >= 2 {
			break
		}
	}
	return out
}

type askItem struct{ ID, Host, Name, Example string }

// OnAsk records a person's answer to "is this worth noticing?" for a pattern the teacher could
// not settle. It becomes the label for laya and the verdict the teacher repeats from now on.
func (b *Bot) OnAsk(ctx context.Context, pid string, odd bool, user string) string {
	text, ok, err := b.Store.LastDecision(ctx, "line", pid)
	if err != nil || !ok {
		return "このログの判断記録が見つかりませんでした。"
	}
	ans := map[string]any{"odd": odd}
	b.Store.Label(ctx, b.Now(), "line", text, ans, laya.OriginHuman, user, true, "asked in digest "+pid)
	b.Store.SetVerdict(ctx, "line", pid, ans)
	if b.Laya != nil {
		if err := b.Laya.Feedback(ctx, "line", text, ans, true, laya.OriginHuman); err != nil {
			b.Log.Warn("ask feedback", "err", err)
		}
	}
	meta, _ := b.Rules.Template(pid)
	word := "気にしなくてよい"
	if odd {
		word = "気にすべき"
	}
	return fmt.Sprintf("ありがとうございます、<@%s> さん。%s の「%s」は「%s」と覚えました。", user, meta.Host, b.friendlyName(ctx, "", pid, meta.Text, 60), word)
}
