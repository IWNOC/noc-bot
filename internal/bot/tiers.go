package bot

// How people hear about things, the way a NOC shift works: by what a log means, not by how many
// lines it produced (people acted on 3 of 69 alerts, all of them meaningful events, none a rate
// spike).
//
//   now     a post of its own: a steady sender going silent; a new pattern laya/the teacher read
//           as an error or a state change and found odd; an error right after a config change on
//           the same host; kinds people asked to hear about at once. Bursts are bundled.
//   digest  one summary every 30 minutes, rendered by code with a one-line LLM gist and menus.
//   silent  stored, never posted: routine noise, kinds people muted, known issues.
//
// Mentions come from rules (silence) and from people ("すぐ知らせて"), never from the LLM.
// People set the tier with buttons; it shows at once and can be undone. Being ignored demotes a
// kind to the digest only for info/noise kinds: with many alerts, silence is fatigue, not a verdict.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/IWNOC/noc-bot/internal/incident"
	"github.com/IWNOC/noc-bot/internal/laya"
	"github.com/IWNOC/noc-bot/internal/llm"
	"github.com/IWNOC/noc-bot/internal/rules"
	"github.com/IWNOC/noc-bot/internal/slackapp"
)

const (
	TierNow    = "now"
	TierDigest = "digest"
	TierSilent = "silent"

	stormWindow    = 10 * time.Minute
	stormMax       = 5 // "now" posts per stormWindow; more are bundled into one post
	ignoreAfter    = time.Hour
	demoteAfterN   = 3
	digestInterval = 30 * time.Minute
	reopenCooldown = 6 * time.Hour
	configWindow   = 10 * time.Minute
)

// tierRule is what people taught about one kind of alert.
type tierRule struct {
	Tier    string    `json:"tier"`
	Ignored int       `json:"ignored"`
	Auto    bool      `json:"auto"` // set by noc-bot from ignored alerts, not by a person
	Who     string    `json:"who"`
	At      time.Time `json:"at"`
	Label   string    `json:"label"`
	Prev    string    `json:"prev"` // tier before the last change, for undo
}

// tierKey: alerts "of the same kind" share the log pattern, or else kind and host.
func tierKey(in incident.Incident) string {
	if in.TplID != "" {
		return "tpl:" + in.TplID
	}
	return in.Kind + ":" + in.Host
}

func tierLabel(in incident.Incident) string {
	if in.Title != "" { // the headline people saw, not the raw pattern
		return in.Host + " の「" + clipText(in.Title, 60) + "」"
	}
	if in.Template != "" {
		return in.Host + " の「" + clipText(in.Template, 60) + "」"
	}
	return in.Host + " の " + in.Kind
}

func (b *Bot) tierRules(ctx context.Context) map[string]tierRule {
	m := map[string]tierRule{}
	b.Store.GetJSON(ctx, "tiers", &m)
	return m
}

func (b *Bot) setTierRule(ctx context.Context, key string, f func(*tierRule)) tierRule {
	b.tierMu.Lock()
	defer b.tierMu.Unlock()
	m := b.tierRules(ctx)
	r := m[key]
	f(&r)
	m[key] = r
	b.Store.PutJSON(ctx, "tiers", m)
	return r
}

// lineView is what the line desk concluded about a template (only answers we trust).
type lineView struct {
	Kind string // error | state_change | info | noise | ""
	Odd  bool
}

func (b *Bot) lineView(ctx context.Context, tplID string) lineView {
	var v lineView
	if tplID == "" {
		return v
	}
	ans, ok := b.Store.LastAnswers(ctx, "line", tplID)
	if !ok {
		return v
	}
	read := func(q string) (any, bool) {
		raw, ok := ans[q]
		if !ok {
			return nil, false
		}
		var a laya.Answer
		if json.Unmarshal(raw, &a) != nil || !trusted(a) {
			return nil, false
		}
		return a.Answer, true
	}
	if k, ok := read("kind"); ok {
		v.Kind, _ = k.(string)
	}
	if o, ok := read("odd"); ok {
		v.Odd = o == true || o == "true"
	}
	return v
}

var configChangeRe = regexp.MustCompile(`(?i)CONFIG_I|Configured from|configured by|commit(ted)?\b|config(uration)? (change|saved|applied)`)

// noteConfigChange remembers when a host's configuration changed (Arista CONFIG_I, VyOS commit).
func (b *Bot) noteConfigChange(host, msg string, at time.Time) {
	if !configChangeRe.MatchString(msg) {
		return
	}
	b.tierMu.Lock()
	b.configAt[host] = at
	b.tierMu.Unlock()
}

func (b *Bot) changedRecently(host string, at time.Time) bool {
	b.tierMu.Lock()
	defer b.tierMu.Unlock()
	t, ok := b.configAt[host]
	return ok && at.Sub(t) >= 0 && at.Sub(t) <= configWindow
}

// decideTier picks how people hear about a new incident, and why (shown in the post).
func (b *Bot) decideTier(ctx context.Context, in incident.Incident, s rules.Signal) (tier, why string, page bool) {
	if r, ok := b.tierRules(ctx)[tierKey(in)]; ok && r.Tier != "" {
		if r.Auto {
			return r.Tier, "反応がなかった種類なのでまとめに回しています", false
		}
		return r.Tier, "以前この種類を「" + map[string]string{TierNow: "すぐ知らせて", TierDigest: "まとめでいい", TierSilent: "通知不要"}[r.Tier] + "」にしてもらったため", r.Tier == TierNow
	}
	if s.Kind == rules.KindSilence {
		return TierNow, "いつも送ってくる送信元から届かなくなったため", true
	}
	if s.Kind == rules.KindClockSkew {
		return TierDigest, "", false
	}
	lv := b.lineView(ctx, in.TplID)
	switch lv.Kind {
	case "noise":
		return TierSilent, "", false
	case "info":
		return TierDigest, "", false
	case "error", "state_change":
		if ps := b.zabbixOn(ctx, in.Host, in.Opened); len(ps) > 0 {
			return TierNow, "Zabbix でも同じ機器に障害が出ているため（" + ps[0].Name + "）", false
		}
		if b.changedRecently(in.Host, in.Opened) {
			return TierNow, "同じ機器の設定変更の直後に出たため", false
		}
		if s.Kind == "odd" || (s.Kind == rules.KindRate && lv.Odd) {
			return TierNow, "エラー・状態変化で、いつもと違うと判断したため", false
		}
	}
	return TierDigest, "", false
}

// stormy reports whether too many "now" posts went out recently; if so the caller bundles.
func (b *Bot) stormy(now time.Time) bool {
	b.tierMu.Lock()
	defer b.tierMu.Unlock()
	kept := b.nowPosts[:0]
	for _, t := range b.nowPosts {
		if now.Sub(t) < stormWindow {
			kept = append(kept, t)
		}
	}
	b.nowPosts = kept
	if len(b.nowPosts) >= stormMax {
		return true
	}
	b.nowPosts = append(b.nowPosts, now)
	return false
}

// flushStorm posts bundled "now" alerts as one message (called every minute).
func (b *Bot) flushStorm(ctx context.Context) {
	b.tierMu.Lock()
	q := b.stormQ
	b.stormQ = nil
	b.tierMu.Unlock()
	if len(q) == 0 {
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "🔴 *短い時間に %d 件、すぐ知らせるべき検知が重なりました*\n", len(q))
	for _, in := range q {
		t := in.Title
		if t == "" {
			t = clipText(in.Template, 90)
		}
		fmt.Fprintf(&sb, "• %s：%s（%s）\n", in.Host, t, in.ID)
	}
	sb.WriteString("_個別の詳細はこのスレッドで聞いてください。_")
	ts, err := b.Notify.Post(ctx, sb.String(), "")
	if err != nil {
		b.Log.Warn("storm post", "err", err)
		return
	}
	for _, in := range q {
		b.Incidents.Update(in.Key, func(i *incident.Incident) {
			if i.Thread == "" {
				i.Thread = ts
			}
			i.Notified = b.Now()
		})
	}
}

// sameHostWindow: alerts on one host this close together are one story (a DHCP failure brings
// resolver and reload messages with it); they share a thread.
const sameHostWindow = 15 * time.Minute

// relatedThread finds where a new "now" alert belongs: the thread of the same kind within the
// cooldown (a recurrence), or of an alert on the same host in the last few minutes (same story).
func (b *Bot) relatedThread(in incident.Incident) (thread, why string) {
	if t := b.previousThread(in); t != "" {
		return t, "再発（前回のスレッドに追記）"
	}
	var best incident.Incident
	for _, o := range b.Incidents.All() {
		if o.ID == in.ID || o.Thread == "" || o.Tier != TierNow || o.Host != in.Host || in.Opened.Sub(o.Notified) > sameHostWindow {
			continue
		}
		if o.Notified.After(best.Notified) {
			best = o
		}
	}
	if best.Thread != "" {
		return best.Thread, "同じ機器で直前に知らせた件（" + best.ID + "）の続き"
	}
	return "", ""
}

// previousThread finds the thread of the last posted incident of the same kind within the
// cooldown, so a recurrence is added there instead of a new post.
func (b *Bot) previousThread(in incident.Incident) string {
	key := tierKey(in)
	var best incident.Incident
	for _, o := range b.Incidents.All() {
		if o.ID == in.ID || o.Thread == "" || tierKey(o) != key || in.Opened.Sub(o.LastSeen) > reopenCooldown {
			continue
		}
		if o.Opened.After(best.Opened) {
			best = o
		}
	}
	return best.Thread
}

// attended marks that a person reacted (and resets the ignored streak).
func (b *Bot) attended(ctx context.Context, in incident.Incident) {
	b.Incidents.Update(in.Key, func(i *incident.Incident) { i.Attended = true })
	b.setTierRule(ctx, tierKey(in), func(r *tierRule) { r.Ignored = 0 })
}

// judgeAttention: "now" alerts of info/noise kinds nobody reacted to for an hour count as ignored;
// three in a row move that kind to the digest (shown there, reversible). Errors and state changes
// are never demoted without a person.
func (b *Bot) judgeAttention(ctx context.Context, now time.Time) {
	for _, in := range b.Incidents.All() {
		if in.Tier != TierNow || in.Thread == "" || in.Judged || in.Attended || in.Notified.IsZero() || now.Sub(in.Notified) < ignoreAfter {
			continue
		}
		b.Incidents.Update(in.Key, func(i *incident.Incident) { i.Judged = true })
		kind := b.lineView(ctx, in.TplID).Kind
		b.Store.Note(ctx, now, "ignored", tierKey(in), in.ID, "noc-bot")
		if kind != "info" && kind != "noise" {
			continue
		}
		b.setTierRule(ctx, tierKey(in), func(r *tierRule) {
			r.Ignored++
			r.Label = tierLabel(in)
			if r.Ignored >= demoteAfterN && r.Who == "" && r.Tier != TierDigest {
				r.Prev, r.Tier, r.Auto, r.At = r.Tier, TierDigest, true, now
			}
		})
	}
}

// setTierFromPerson applies a person's choice, remembers the previous tier for undo, and records
// it as a policy label (not a laya label: tiers are decided by code and people).
func (b *Bot) setTierFromPerson(ctx context.Context, in incident.Incident, tier, user string) tierRule {
	b.attended(ctx, in)
	r := b.setTierRule(ctx, tierKey(in), func(r *tierRule) {
		prev := r.Tier
		if prev == "" {
			prev = in.Tier
		}
		r.Prev, r.Tier, r.Auto, r.Who, r.At, r.Label, r.Ignored = prev, tier, false, user, b.Now(), tierLabel(in), 0
	})
	b.Store.Note(ctx, b.Now(), "tier", tierKey(in), tier+" "+in.ID, user)
	return r
}

// undoTier puts a kind back to what it was before the last change.
func (b *Bot) undoTier(ctx context.Context, key, user string) string {
	var label string
	b.setTierRule(ctx, key, func(r *tierRule) {
		label = r.Label
		r.Tier, r.Prev, r.Auto, r.Who, r.Ignored = r.Prev, "", false, "", 0
	})
	b.Store.Note(ctx, b.Now(), "tier", key, "undo", user)
	return fmt.Sprintf("↩️ <@%s> が元に戻しました：%s", user, label)
}

// ---------------------------------------------------------------- the digest

// DigestPoster posts the digest with promote/mute menus.
type DigestPoster interface {
	PostDigest(ctx context.Context, text string, items, asks []slackapp.DigestItem) (string, error)
}

const digestSystem = `あなたはイベントネットワークの NOC の運用担当です。この30分の項目一覧を見て、全体の所感を「1行」で書きます。
落ち着いているか、気にすべき流れがあるか。数字・IP・時刻は一覧にあるものだけ。JSON だけで答える：{"gist": "<1行>"}`

func (b *Bot) digest(ctx context.Context, now time.Time) {
	var items []incident.Incident
	for _, in := range b.Incidents.All() {
		if in.Tier == TierDigest && (in.Digested.IsZero() || in.LastSeen.After(in.Digested)) {
			items = append(items, in)
		}
	}
	demoted := b.recentlyDemoted(ctx, now)
	findings := b.takeFindings()
	asks := b.asks(ctx, now)
	if len(items) == 0 && len(demoted) == 0 && len(findings) == 0 && len(asks) == 0 {
		return
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Host != items[j].Host {
			return items[i].Host < items[j].Host
		}
		return items[i].Hits > items[j].Hits
	})
	var list strings.Builder
	byHost := map[string]int{}
	for _, in := range items {
		byHost[in.Host]++
		title := b.friendlyName(ctx, in.Title, in.TplID, in.Template, 90)
		fmt.Fprintf(&list, "• %s：%s（%d回）\n", in.Host, title, in.Hits)
	}
	gist := ""
	if lm := b.agentLLM(); lm != nil && len(items) > 0 {
		cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		m, err := lm.Chat(cctx, []llm.Message{{Role: "system", Content: digestSystem}, {Role: "user", Content: list.String()}}, nil, 400, true)
		cancel()
		var got struct{ Gist string }
		if err == nil && json.Unmarshal([]byte(jsonPart(m.Content)), &got) == nil && len(ungrounded(got.Gist, list.String())) == 0 {
			gist = strings.TrimSpace(got.Gist)
		}
	}
	var sb strings.Builder
	span := now.Add(-digestInterval).In(jst).Format("15:04") + "〜" + now.In(jst).Format("15:04")
	if len(items) > 0 {
		fmt.Fprintf(&sb, "🗒 *この30分のまとめ*（%s、%d件）\n", span, len(items))
	} else {
		fmt.Fprintf(&sb, "🗒 *この30分*（%s）：通知するほどの出来事はありませんでした。\n", span)
	}
	if gist != "" {
		sb.WriteString(gist + "\n")
	}
	lines := strings.Split(strings.TrimRight(list.String(), "\n"), "\n")
	if len(lines) > 20 {
		lines = append(lines[:20], fmt.Sprintf("…ほか %d 件", len(items)-20))
	}
	if len(items) > 0 {
		sb.WriteString(strings.Join(lines, "\n") + "\n")
	}
	if len(findings) > 0 {
		sb.WriteString("\n🔎 *調べてわかったこと*（AI の調査・誤りの可能性あり）\n")
		for _, f := range findings {
			sb.WriteString("• " + f + "\n")
		}
	}
	var askMenu []slackapp.DigestItem
	if len(asks) > 0 {
		sb.WriteString("\n🙋 *判断に迷ったログ*（下のメニューで教えてもらえると、次から自分で判断できます）\n")
		for _, a := range asks {
			fmt.Fprintf(&sb, "• %s：%s\n   例：`%s`\n", a.Host, a.Name, a.Example)
			askMenu = append(askMenu, slackapp.DigestItem{ID: a.ID, Label: clipText(a.Host+" "+a.Name, 70)})
		}
	}
	if len(demoted) > 0 {
		sb.WriteString("_反応がなかったので、次の種類はまとめに回すようにしました（メニューの「すぐ知らせて」で戻せます）：_ " + strings.Join(demoted, "、") + "\n")
	}
	var menu []slackapp.DigestItem
	for i, in := range items {
		if i >= 25 {
			break
		}
		menu = append(menu, slackapp.DigestItem{ID: in.ID, Label: clipText(in.Host+" "+b.friendlyName(ctx, in.Title, in.TplID, in.Template, 60), 70)})
	}
	var ts string
	var err error
	if dp, ok := b.Notify.(DigestPoster); ok {
		ts, err = dp.PostDigest(ctx, sb.String(), menu, askMenu)
	} else {
		ts, err = b.Notify.Post(ctx, sb.String(), "")
	}
	if err != nil {
		b.Log.Warn("digest post", "err", err)
		return
	}
	for _, in := range items {
		b.Incidents.Update(in.Key, func(i *incident.Incident) {
			i.Digested = now
			if i.Thread == "" {
				i.Thread = ts
			}
		})
	}
}

func (b *Bot) recentlyDemoted(ctx context.Context, now time.Time) []string {
	var out []string
	for _, r := range b.tierRules(ctx) {
		if r.Auto && r.Tier == TierDigest && now.Sub(r.At) < digestInterval+time.Minute {
			out = append(out, r.Label)
		}
	}
	sort.Strings(out)
	return out
}
