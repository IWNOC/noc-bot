package bot

// Answers people give on alerts (buttons, emoji) and the site knowledge they confirm.
// Conversation itself lives in agent.go.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/IWNOC/noc-bot/internal/config"
	"github.com/IWNOC/noc-bot/internal/incident"
	"github.com/IWNOC/noc-bot/internal/laya"
	"github.com/IWNOC/noc-bot/internal/slackapp"
)

// IncidentPoster is a notifier that can put answer buttons on an alert.
type IncidentPoster interface {
	PostIncident(ctx context.Context, incidentID, text, thread string) (string, error)
}

// Proposer can post "should I learn this?" cards and answer in any thread.
type Proposer interface {
	PostProposal(ctx context.Context, thread, proposalID, text string) error
	PostProposalIn(ctx context.Context, channel, thread, proposalID, text string) error
	PostIn(ctx context.Context, channel, thread, text string) error
	Post(ctx context.Context, text, thread string) (string, error)
}

// Conversation reads a thread's history.
type Conversation interface {
	Thread(ctx context.Context, channel, ts string) ([]slackapp.Turn, error)
}

// Network areas: what laya is asked (stable across events). Which team owns an area is site
// knowledge, learned in conversation (team.Areas).
var areas = map[string]bool{"physical": true, "l2l3": true, "server": true, "auth": true, "wireless": true}

// teamForArea finds the team that owns an area, if one has been set up.
func (b *Bot) teamForArea(area string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	names := make([]string, 0, len(b.teams))
	for k := range b.teams {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, a := range b.teams[n].Areas {
			if a == area {
				return n
			}
		}
	}
	return ""
}

// areaOfTeam is the first area a team owns ("" if none set).
func (b *Bot) areaOfTeam(team string) string {
	t, ok := b.team(team)
	if !ok || len(t.Areas) == 0 {
		return ""
	}
	return t.Areas[0]
}

// ---------------------------------------------------------------- teams and senders

func (b *Bot) team(name string) (config.Team, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.teams[name]
	return t, ok
}

// TeamNames lists team ids for the "担当違う" menu.
func (b *Bot) TeamNames() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.teams))
	for k := range b.teams {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (b *Bot) senderFor(from, host string) config.Sender {
	b.mu.Lock()
	extra := append([]config.Sender(nil), b.senders...)
	b.mu.Unlock()
	for _, s := range extra {
		if (s.From != "" && from != "" && config.Glob(s.From, from)) || (s.Host != "" && config.Glob(s.Host, host)) {
			base := b.Cfg.SenderFor(from, host)
			if s.Team != "" {
				base.Team = s.Team
			}
			return base
		}
	}
	return b.Cfg.SenderFor(from, host)
}

// loadLearned restores known issues, teams and sender rules people added in Slack.
func (b *Bot) loadLearned(ctx context.Context) {
	var known []config.KnownIssue
	if ok, err := b.Store.GetJSON(ctx, "known_issues", &known); ok && err == nil {
		for _, k := range known {
			if err := b.Rules.AddKnown(k); err != nil {
				b.Log.Warn("restore known issue", "id", k.ID, "err", err)
			}
		}
	}
	var teams map[string]config.Team
	if ok, err := b.Store.GetJSON(ctx, "teams", &teams); ok && err == nil {
		for k, v := range teams {
			b.teams[k] = v
		}
	}
	var senders []config.Sender
	if ok, err := b.Store.GetJSON(ctx, "senders", &senders); ok && err == nil {
		b.senders = senders
	}
}

// ---------------------------------------------------------------- buttons and emoji

// OnAnswer handles the buttons and menus on alerts and digests. Tier buttons change how this kind
// of alert reaches people (visible at once, undoable); the verdict and the menus are labels for laya.
func (b *Bot) OnAnswer(ctx context.Context, incidentID, action, value, user string) (string, string) {
	in, ok := b.Incidents.ByID(incidentID)
	if !ok {
		return "この件はもう記録にありません（noc-bot の再起動前のものかもしれません）。", ""
	}
	b.attended(ctx, in)
	tierReply := func(tier, msg string) (string, string) {
		b.setTierFromPerson(ctx, in, tier, user)
		return fmt.Sprintf("<@%s>：%s を%s", user, tierLabel(in), msg), tierKey(in)
	}
	switch action {
	case slackapp.ActNow, slackapp.ActRight, slackapp.ActPromote:
		// "worth telling at once": a strong positive for laya too
		b.teach(ctx, in, map[string]any{"odd": true}, nil, false, user)
		return tierReply(TierNow, "、これからはすぐ知らせます。")
	case slackapp.ActDigest:
		return tierReply(TierDigest, "、これからは30分ごとのまとめに載せます。")
	case slackapp.ActMute, slackapp.ActMuteSel:
		return tierReply(TierSilent, "、これからは通知しません（記録と件数は残します）。")
	case slackapp.ActWrong:
		n := b.teach(ctx, in, map[string]any{"odd": false}, map[string]any{"page_now": false, "severity": 0}, true, user)
		b.setTierFromPerson(ctx, in, TierSilent, user)
		msg := fmt.Sprintf("<@%s>：誤検知として学習に登録し、%s は今後通知しません。", user, tierLabel(in))
		if n == 0 {
			msg = fmt.Sprintf("<@%s>：%s は今後通知しません（laya の判断記録がないため学習には使っていません）。", user, tierLabel(in))
		}
		return msg, tierKey(in)
	case slackapp.ActSeverity, slackapp.ActSevUp, slackapp.ActSevDown:
		sev := in.Severity
		switch action {
		case slackapp.ActSeverity:
			fmt.Sscan(value, &sev)
		case slackapp.ActSevUp:
			if sev < 3 {
				sev++
			}
		case slackapp.ActSevDown:
			if sev > 0 {
				sev--
			}
		}
		if sev < 0 || sev > 3 {
			return "", ""
		}
		b.Incidents.Update(in.Key, func(i *incident.Incident) { i.Severity = sev })
		b.teach(ctx, in, nil, map[string]any{"severity": sev}, true, user)
		return fmt.Sprintf("📝 <@%s>：影響の大きさ %d/3 を学習に登録しました。", user, sev), ""
	case slackapp.ActTeam:
		b.Incidents.Update(in.Key, func(i *incident.Incident) { i.Team = value })
		lineAns, incAns := map[string]any{}, map[string]any{}
		if a := b.areaOfTeam(value); a != "" {
			lineAns["area"], incAns["area"] = a, a
		}
		b.teach(ctx, in, lineAns, incAns, true, user)
		return fmt.Sprintf("📝 <@%s>：担当は %s として登録しました。", user, value), ""
	}
	return "", ""
}

// OnUndo reverts a tier change.
func (b *Bot) OnUndo(ctx context.Context, key, user string) string {
	return b.undoTier(ctx, key, user)
}

// teach sends human answers to the desks that decided this incident. It returns how many desks learned.
func (b *Bot) teach(ctx context.Context, in incident.Incident, lineAns, incAns map[string]any, correction bool, user string) int {
	n := 0
	try := func(desk, subject string, ans map[string]any) {
		if len(ans) == 0 || subject == "" {
			return
		}
		text, ok, err := b.Store.LastDecision(ctx, desk, subject)
		if err != nil || !ok {
			return
		}
		b.Store.Label(ctx, b.Now(), desk, text, ans, laya.OriginHuman, user, correction, "incident "+in.ID)
		if b.Laya == nil {
			return
		}
		if err := b.Laya.Feedback(ctx, desk, text, ans, correction, laya.OriginHuman); err != nil {
			b.Log.Warn("human feedback", "desk", desk, "err", err)
			return
		}
		n++
	}
	try("incident", in.ID, incAns)
	if in.TplID != "" { // rate alerts too: a false alarm on a pattern must teach the line desk
		try("line", in.TplID, lineAns)
	}
	return n
}

// OnReaction maps ✅ / ❌ on an alert to the same answers as the buttons.
func (b *Bot) OnReaction(ctx context.Context, messageTS, emoji, user string) {
	in, ok := b.Incidents.ByThread(messageTS)
	if !ok {
		return
	}
	action := ""
	switch emoji {
	case "white_check_mark", "heavy_check_mark", "ok", "+1":
		action = slackapp.ActNow
	case "x", "negative_squared_cross_mark", "-1":
		action = slackapp.ActWrong
	default:
		return
	}
	if reply, _ := b.OnAnswer(ctx, in.ID, action, "", user); reply != "" {
		b.Notify.Post(ctx, reply, in.Thread)
	}
}

// knownFrom turns a confirmed comment into a known issue scoped to what the person chose.
func (b *Bot) knownFrom(in incident.Incident, reason, scope string, days int, user string) config.KnownIssue {
	meta, _ := b.Rules.Template(in.TplID)
	k := config.KnownIssue{ID: "ki-" + newID()[:6], Reason: reason + "（" + user + " が Slack で登録）",
		Expires: b.Now().Add(time.Duration(days) * 24 * time.Hour), Host: in.Host}
	switch scope {
	case "host":
		k.Template = ".*"
	case "program":
		k.Program, k.Template = meta.Program, ".*"
	default:
		k.Program, k.Template = meta.Program, templateRegexp(in.Template)
	}
	if in.Kind == "clock_skew" {
		// a clock that is off by a fixed amount: accept that amount, not the whole host
		k.Template = ""
		if d, err := time.ParseDuration(in.Facts["skew"]); err == nil {
			k.ExpectSkew, k.SkewTolerance = d, 2*time.Minute
		}
	}
	return k
}

// templateRegexp matches the template's text with <*> and placeholders as wildcards.
func templateRegexp(tpl string) string {
	parts := regexp.MustCompile(`<[A-Z0-9*]+>`).Split(tpl, -1)
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return "^" + strings.Join(parts, ".*") + "$"
}

func (b *Bot) addKnown(ctx context.Context, k config.KnownIssue) error {
	if err := b.Rules.AddKnown(k); err != nil {
		return err
	}
	var all []config.KnownIssue
	b.Store.GetJSON(ctx, "known_issues", &all)
	all = append(all, k)
	return b.Store.PutJSON(ctx, "known_issues", all)
}

// Directory lists Slack user groups, so "@team-server" typed as plain text can still be resolved.
type Directory interface {
	Groups(ctx context.Context) ([]slackapp.Group, error)
}

func (b *Bot) groups(ctx context.Context) []slackapp.Group {
	d, ok := b.Notify.(Directory)
	if !ok {
		return nil
	}
	gs, err := d.Groups(ctx)
	if err != nil {
		b.Log.Warn("slack user groups", "err", err)
	}
	return gs
}

func (b *Bot) describeTeams() string {
	var sb strings.Builder
	names := b.TeamNames()
	if len(names) == 0 {
		return "teams: none yet (nobody has told noc-bot who owns what)"
	}
	sb.WriteString("teams:\n")
	for _, name := range names {
		t, _ := b.team(name)
		m := t.Mention
		if m == "" {
			m = "(no mention set)"
		}
		fmt.Fprintf(&sb, "- %s mention=%s owns=%s\n", name, m, t.Description)
	}
	b.mu.Lock()
	for _, s := range b.senders {
		fmt.Fprintf(&sb, "  host rule: %s -> %s\n", s.Host, s.Team)
	}
	b.mu.Unlock()
	return sb.String()
}

func (b *Bot) knownHosts() []string {
	var out []string
	for _, s := range b.Stats.All("sender", b.Now()) {
		out = append(out, s.Key.ID)
		if len(out) >= 60 {
			break
		}
	}
	return out
}

func newID() string {
	var x [8]byte
	rand.Read(x[:])
	return hex.EncodeToString(x[:])
}

func jsonPart(s string) string {
	i, j := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}')
	if i < 0 || j < i {
		return s
	}
	return s[i : j+1]
}

func (b *Bot) postCardIn(ctx context.Context, channel, thread, id, text string) {
	if pr, ok := b.Notify.(Proposer); ok {
		if err := pr.PostProposalIn(ctx, channel, thread, id, text); err != nil {
			b.Log.Warn("post card", "err", err)
		}
	}
}
