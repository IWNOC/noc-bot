package bot

// What people tell noc-bot in Slack, and how it becomes learning.
//
//   buttons / emoji on an alert      -> human labels to laya (line + incident desks), stored
//   a reply in an incident thread     -> comment desk + the LLM reads it into a proposal
//                                        ("こう理解しました"); only what a person accepts is learned
//   a request in the thread           -> the LLM investigates with read-only tools and answers
//   @noc-bot with @usergroups         -> team setup, proposed and confirmed the same way
//
// Nothing an LLM extracts is learned or applied until a person presses 登録.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/IWNOC/noc-bot/internal/config"
	"github.com/IWNOC/noc-bot/internal/incident"
	"github.com/IWNOC/noc-bot/internal/laya"
	"github.com/IWNOC/noc-bot/internal/llm"
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

// Laya's option names: answers outside them are not sent (laya-svc would refuse them).
var (
	lineTeams     = map[string]bool{"l2l3": true, "server": true, "auth": true, "wifi": true, "none": true}
	incidentTeams = map[string]bool{"l2l3": true, "server": true, "auth": true, "wifi": true, "external": true}
)

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

// OnAnswer handles the answer buttons on an alert.
func (b *Bot) OnAnswer(ctx context.Context, incidentID, action, value, user string) string {
	in, ok := b.Incidents.ByID(incidentID)
	if !ok {
		return "この障害はもう記録にありません（noc-bot の再起動前のものかもしれません）。"
	}
	lineAns, incAns := map[string]any{}, map[string]any{}
	correction := true
	what := ""
	switch action {
	case slackapp.ActRight:
		correction, what = false, "合っている"
		lineAns["odd"] = true
		if in.Team != "" && incidentTeams[in.Team] {
			incAns["primary_team"] = in.Team
		}
		if in.Severity >= 0 {
			incAns["severity"] = in.Severity
		}
	case slackapp.ActWrong:
		what = "誤検知"
		lineAns["odd"] = false
		incAns["page_now"] = false
		incAns["severity"] = 0
	case slackapp.ActSevUp, slackapp.ActSevDown:
		sev := in.Severity
		if sev < 0 {
			sev = 1
		}
		if action == slackapp.ActSevUp && sev < 3 {
			sev++
		} else if action == slackapp.ActSevDown && sev > 0 {
			sev--
		}
		what = fmt.Sprintf("重要度 %d/3", sev)
		incAns["severity"] = sev
		if action == slackapp.ActSevUp {
			lineAns["odd"] = true
		}
		b.Incidents.Update(in.Key, func(i *incident.Incident) { i.Severity = sev })
	case slackapp.ActTeam:
		what = "担当は " + value
		if lineTeams[value] {
			lineAns["team"] = value
		}
		if incidentTeams[value] {
			incAns["primary_team"] = value
		}
		b.Incidents.Update(in.Key, func(i *incident.Incident) { i.Team = value })
	default:
		return ""
	}
	n := b.teach(ctx, in, lineAns, incAns, correction, user)
	if n == 0 {
		return fmt.Sprintf("📝 <@%s> の判定（%s）を記録しました。laya がこの件を判断した記録がないため、学習には使っていません。", user, what)
	}
	return fmt.Sprintf("📝 <@%s> の判定（%s）を学習に登録しました。", user, what)
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
	if in.Kind == "odd" {
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
		action = slackapp.ActRight
	case "x", "negative_squared_cross_mark", "-1":
		action = slackapp.ActWrong
	default:
		return
	}
	if reply := b.OnAnswer(ctx, in.ID, action, "", user); reply != "" {
		b.Notify.Post(ctx, reply, in.Thread)
	}
}

// ---------------------------------------------------------------- comments -> proposals

type proposal struct {
	ID       string
	Incident incident.Incident
	Comment  string
	User     string
	Thread   string
	Channel  string
	Read     reading
	Teams    []teamSetup
	At       time.Time
}

// reading is what the LLM understood from one comment.
type reading struct {
	Summary    string `json:"summary_ja"`
	Correct    *bool  `json:"correct"`
	Severity   *int   `json:"severity"`
	Team       string `json:"team"`
	KnownIssue *struct {
		Reason string `json:"reason"`
		Scope  string `json:"scope"` // template | host | program
		Days   int    `json:"days"`
	} `json:"known_issue"`
	Fact        string `json:"fact"`
	Resolution  string `json:"resolution"`
	Instruction string `json:"instruction"`
}

func (r reading) learnable() bool {
	return r.Correct != nil || r.Severity != nil || r.Team != "" || r.KnownIssue != nil || r.Fact != "" || r.Resolution != ""
}

func (r reading) intent() string {
	switch {
	case r.KnownIssue != nil:
		return "known_issue"
	case r.Resolution != "":
		return "resolution"
	case r.Correct != nil || r.Severity != nil || r.Team != "":
		return "label"
	case r.Fact != "":
		return "fact"
	case r.Instruction != "":
		return "instruction"
	}
	return "chat"
}

const readSystem = `You read one comment an operator wrote in the Slack thread of a network alert, and say what it teaches.
Reply with JSON only:
{"summary_ja": "<what you understood, in Japanese, one or two sentences>",
 "correct": true|false|null,           // the alert was right (true) or a false alarm (false); null if not said
 "severity": 0|1|2|3|null,             // impact the operator states: 0 none, 1 a few users, 2 many, 3 everyone
 "team": "l2l3"|"server"|"auth"|"wifi"|"",  // team the operator says owns it
 "known_issue": {"reason": "<why it is accepted, Japanese>", "scope": "template"|"host"|"program", "days": <how long>} | null,
 "fact": "<a lasting fact about the network the operator states, Japanese>" | "",
 "resolution": "<the cause or fix the operator reports, Japanese>" | "",
 "instruction": "<what the operator asks the bot to check now, Japanese>" | ""}
Only fill what the comment actually says. "known_issue" only when the operator says it is known, accepted,
expected, or will not be fixed. scope "template" = this exact log pattern; "host" = everything from this host;
"program" = this program on this host. days: what they say, else 3.`

// OnThreadReply handles a person's reply in an incident thread.
func (b *Bot) OnThreadReply(ctx context.Context, threadTS, text, user string) {
	in, ok := b.Incidents.ByThread(threadTS)
	if !ok {
		return
	}
	b.Store.Note(ctx, b.Now(), "comment", in.ID, text, user)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	commentText := fmt.Sprintf("Alert: %s on %s. Pattern: %s\nOperator wrote: %s", in.Kind, in.Host, in.Template, text)
	if b.Laya != nil {
		if d, err := b.Laya.Decide(ctx, "comment", commentText); err == nil {
			b.record(ctx, "comment", in.ID, commentText, d)
		}
	}
	if b.LLM == nil {
		return
	}
	r, err := b.readComment(ctx, in, text)
	if err != nil {
		b.Log.Warn("read comment", "err", err)
		return
	}
	if r.Instruction != "" {
		go b.investigate(context.Background(), in, r.Instruction, "", in.Thread)
	}
	if !r.learnable() {
		return
	}
	p := &proposal{ID: newID(), Incident: in, Comment: commentText, User: user, Thread: in.Thread, Read: r, At: b.Now()}
	b.mu.Lock()
	b.props[p.ID] = p
	b.mu.Unlock()
	pr, ok := b.Notify.(Proposer)
	if !ok {
		return
	}
	if err := pr.PostProposal(ctx, in.Thread, p.ID, describe(p)); err != nil {
		b.Log.Warn("post proposal", "err", err)
	}
}

func (b *Bot) readComment(ctx context.Context, in incident.Incident, text string) (reading, error) {
	user := fmt.Sprintf("Alert kind: %s\nHost: %s\nLog pattern: %s\nFacts: %v\nOperator's comment:\n%s", in.Kind, in.Host, in.Template, in.Facts, text)
	m, err := b.LLM.Chat(ctx, []llm.Message{{Role: "system", Content: readSystem}, {Role: "user", Content: user}}, nil, 800, true)
	if err != nil {
		return reading{}, err
	}
	out := m.Content
	var r reading
	if err := json.Unmarshal([]byte(jsonPart(out)), &r); err != nil {
		return reading{}, fmt.Errorf("unparsable reading: %.200s", out)
	}
	if r.Team != "" && !lineTeams[r.Team] && !incidentTeams[r.Team] {
		r.Team = ""
	}
	if r.Severity != nil && (*r.Severity < 0 || *r.Severity > 3) {
		r.Severity = nil
	}
	if r.KnownIssue != nil {
		if r.KnownIssue.Days <= 0 || r.KnownIssue.Days > 30 {
			r.KnownIssue.Days = 3
		}
		switch r.KnownIssue.Scope {
		case "template", "host", "program":
		default:
			r.KnownIssue.Scope = "template"
		}
	}
	return r, nil
}

func describe(p *proposal) string {
	r := p.Read
	var sb strings.Builder
	fmt.Fprintf(&sb, "📝 *こう理解しました*（<@%s> のコメントから）\n> %s\n", p.User, r.Summary)
	if r.Correct != nil {
		if *r.Correct {
			sb.WriteString("• 判定：この通知は合っている\n")
		} else {
			sb.WriteString("• 判定：誤検知\n")
		}
	}
	if r.Severity != nil {
		fmt.Fprintf(&sb, "• 重要度：%d/3\n", *r.Severity)
	}
	if r.Team != "" {
		fmt.Fprintf(&sb, "• 担当：%s\n", r.Team)
	}
	if k := r.KnownIssue; k != nil {
		scope := map[string]string{"template": "このログのパターンだけ", "host": p.Incident.Host + " からのログ全部", "program": p.Incident.Host + " のこのプログラム"}[k.Scope]
		fmt.Fprintf(&sb, "• *既知の問題として登録*：%s\n  範囲：%s　期限：%d日後まで（期限後は再び通知します）\n", k.Reason, scope, k.Days)
	}
	if r.Fact != "" {
		fmt.Fprintf(&sb, "• 環境の事実：%s\n", r.Fact)
	}
	if r.Resolution != "" {
		fmt.Fprintf(&sb, "• 原因・対応：%s\n", r.Resolution)
	}
	sb.WriteString("登録すると学習に使われます。違っていたら却下して、もう一度書いてください。")
	return sb.String()
}

// OnProposal applies (or drops) a proposal.
func (b *Bot) OnProposal(ctx context.Context, id string, accept bool, user string) string {
	b.mu.Lock()
	p := b.props[id]
	delete(b.props, id)
	b.mu.Unlock()
	if p == nil {
		return "この提案はもう処理済みか、noc-bot の再起動で失われました。"
	}
	if !accept {
		b.Store.Label(ctx, b.Now(), "comment", p.Comment, map[string]any{}, laya.OriginHuman, user, false, "proposal rejected")
		return fmt.Sprintf("🗑 <@%s> が却下しました。学習には使いません。", user)
	}
	if len(p.Teams) > 0 {
		return b.applyTeams(ctx, p, user)
	}
	r := p.Read
	in := p.Incident
	done := []string{}
	// labels
	lineAns, incAns := map[string]any{}, map[string]any{}
	if r.Correct != nil {
		lineAns["odd"] = *r.Correct
		if !*r.Correct {
			incAns["page_now"] = false
		}
	}
	if r.Severity != nil {
		incAns["severity"] = *r.Severity
	}
	if r.Team != "" {
		if lineTeams[r.Team] {
			lineAns["team"] = r.Team
		}
		if incidentTeams[r.Team] {
			incAns["primary_team"] = r.Team
		}
	}
	if r.KnownIssue != nil {
		lineAns["odd"] = false
		incAns["cause_class"] = "known_issue"
	}
	if len(lineAns)+len(incAns) > 0 {
		b.teach(ctx, in, lineAns, incAns, true, user)
		done = append(done, "判定")
	}
	if k := r.KnownIssue; k != nil {
		ki := b.knownFrom(in, k.Reason, k.Scope, k.Days, user)
		if err := b.addKnown(ctx, ki); err != nil {
			return "既知の問題の登録に失敗しました: " + err.Error()
		}
		done = append(done, "既知の問題 "+ki.ID)
	}
	if r.Fact != "" {
		b.Store.Note(ctx, b.Now(), "fact", in.Host, r.Fact, user)
		done = append(done, "環境の事実")
	}
	if r.Resolution != "" {
		b.Store.Note(ctx, b.Now(), "resolution", in.ID, r.Resolution, user)
		done = append(done, "原因・対応")
	}
	// the comment desk learns what kind of comment this was, confirmed by a person
	if b.Laya != nil {
		if err := b.Laya.Feedback(ctx, "comment", p.Comment, map[string]any{"intent": r.intent()}, false, laya.OriginHuman); err != nil {
			b.Log.Warn("comment feedback", "err", err)
		}
	}
	b.Store.Label(ctx, b.Now(), "comment", p.Comment, map[string]any{"intent": r.intent()}, laya.OriginHuman, user, false, "proposal accepted")
	return fmt.Sprintf("✅ <@%s> が登録しました：%s", user, strings.Join(done, "、"))
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

// ---------------------------------------------------------------- investigate on request

const investigateSystem = `あなたはイベントネットワークの NOC の調査担当です。運用者の依頼に、道具で事実を確かめてから日本語で答えてください。
- 道具の結果に書かれていることだけを事実として書く。書かれていないことは「確認できなかった」と書く。
- 数字・IP・時刻は道具の結果から正確に写す。
- 10行以内。最後に、次に人が確かめるとよいことを1行。`

func (b *Bot) investigate(ctx context.Context, in incident.Incident, ask, channel, thread string) {
	if b.LLM == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	user := fmt.Sprintf("依頼: %s\n", ask)
	if in.ID != "" {
		user += fmt.Sprintf("対象の通知: %s（%s）パターン: %s 事実: %v\n", in.Kind, in.Host, in.Template, in.Facts)
	}
	out, steps, err := b.LLM.Run(ctx, []llm.Message{{Role: "system", Content: investigateSystem}, {Role: "user", Content: user}}, b.Tools(), 4, 900, false)
	if err != nil {
		out = "調べている途中で失敗しました: " + err.Error()
	}
	used := make([]string, 0, len(steps))
	for _, s := range steps {
		used = append(used, s.Tool)
	}
	if len(used) > 0 {
		out += "\n_（使った道具: " + strings.Join(used, ", ") + "）_"
	}
	text := "🔎 " + out
	if pr, ok := b.Notify.(Proposer); ok && channel != "" {
		pr.PostIn(ctx, channel, thread, text)
		return
	}
	b.Notify.Post(ctx, text, thread)
}

// ---------------------------------------------------------------- @mentions: team setup and questions

type teamSetup struct {
	Team        string   `json:"team"`
	Mention     string   `json:"mention"`
	Description string   `json:"description"`
	Hosts       []string `json:"hosts"`
}

var subteamRe = regexp.MustCompile(`<!subteam\^([A-Z0-9]+)(?:\|[^>]*)?>`)

const teamsSystem = `An operator tells the NOC bot which Slack user group owns which equipment.
Reply with JSON only: {"teams": [{"team": "<one of: l2l3, server, auth, wifi>", "mention": "<the exact <!subteam^...> token from the message>",
"description": "<what they own, Japanese>", "hosts": ["<hostname globs they named or that clearly match, e.g. eps-rt-*>"]}]}
Use only mention tokens that appear in the message. team: l2l3 = switches, routers, BGP, links; server = servers, Kubernetes, DHCP, DNS, NTP, syslog;
auth = RADIUS, eduroam, OpenRoaming; wifi = access points, wireless controller.`

// OnMention handles "@noc-bot ...".
func (b *Bot) OnMention(ctx context.Context, channel, thread, text, user string) {
	pr, ok := b.Notify.(Proposer)
	if !ok {
		return
	}
	body := strings.TrimSpace(regexp.MustCompile(`<@[A-Z0-9]+>`).ReplaceAllString(text, ""))
	switch {
	case strings.Contains(body, "担当設定") && !subteamRe.MatchString(body):
		pr.PostIn(ctx, channel, thread, b.describeTeams())
	case subteamRe.MatchString(body) && b.LLM != nil:
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		m, err := b.LLM.Chat(ctx, []llm.Message{{Role: "system", Content: teamsSystem}, {Role: "user", Content: body + "\n\nHosts currently sending logs: " + strings.Join(b.knownHosts(), ", ")}}, nil, 800, true)
		out := m.Content
		var got struct {
			Teams []teamSetup `json:"teams"`
		}
		if err != nil || json.Unmarshal([]byte(jsonPart(out)), &got) != nil || len(got.Teams) == 0 {
			pr.PostIn(ctx, channel, thread, "担当の設定として読み取れませんでした。例：「@noc-bot スイッチとルーターは @l2l3、サーバーは @server」")
			return
		}
		valid := got.Teams[:0]
		for _, t := range got.Teams {
			if strings.Contains(body, t.Mention) && subteamRe.MatchString(t.Mention) && (lineTeams[t.Team] || incidentTeams[t.Team]) {
				valid = append(valid, t)
			}
		}
		if len(valid) == 0 {
			pr.PostIn(ctx, channel, thread, "メンション先のグループを読み取れませんでした。")
			return
		}
		p := &proposal{ID: newID(), User: user, Thread: thread, Channel: channel, Teams: valid, At: b.Now()}
		b.mu.Lock()
		b.props[p.ID] = p
		b.mu.Unlock()
		var sb strings.Builder
		fmt.Fprintf(&sb, "📝 *担当の設定を次のように理解しました*（<@%s>）\n", user)
		for _, t := range valid {
			fmt.Fprintf(&sb, "• *%s* → %s：%s", t.Team, t.Mention, t.Description)
			if len(t.Hosts) > 0 {
				fmt.Fprintf(&sb, "（%s）", strings.Join(t.Hosts, ", "))
			}
			sb.WriteString("\n")
		}
		sb.WriteString("判断できないときは、このチャンネルに投稿します（メンションなし）。")
		b.postCardIn(ctx, channel, thread, p.ID, sb.String())
	default:
		if b.LLM != nil && body != "" {
			b.investigate(ctx, incident.Incident{}, body, channel, thread)
		}
	}
}

func (b *Bot) postCardIn(ctx context.Context, channel, thread, id, text string) {
	if pr, ok := b.Notify.(Proposer); ok {
		if err := pr.PostProposalIn(ctx, channel, thread, id, text); err != nil {
			b.Log.Warn("post card", "err", err)
		}
	}
}

func (b *Bot) applyTeams(ctx context.Context, p *proposal, user string) string {
	b.mu.Lock()
	for _, t := range p.Teams {
		cur := b.teams[t.Team]
		cur.Mention = subteamRe.FindString(t.Mention)
		if t.Description != "" {
			cur.Description = t.Description
		}
		b.teams[t.Team] = cur
		for _, h := range t.Hosts {
			b.senders = append(b.senders, config.Sender{Host: h, Team: t.Team})
		}
	}
	teams := make(map[string]config.Team, len(b.teams))
	for k, v := range b.teams {
		teams[k] = v
	}
	senders := append([]config.Sender(nil), b.senders...)
	b.mu.Unlock()
	b.Store.PutJSON(ctx, "teams", teams)
	b.Store.PutJSON(ctx, "senders", senders)
	b.Store.Note(ctx, b.Now(), "teams", "slack", fmt.Sprintf("%+v", p.Teams), user)
	return fmt.Sprintf("✅ <@%s> が担当の設定を登録しました。今の設定は「@noc-bot 担当設定」で見られます。", user)
}

func (b *Bot) describeTeams() string {
	var sb strings.Builder
	sb.WriteString("*今の担当設定*\n")
	for _, name := range b.TeamNames() {
		t, _ := b.team(name)
		m := t.Mention
		if m == "" || strings.Contains(m, "XXXX") || strings.Contains(m, "YYYY") {
			m = "（メンション未設定）"
		}
		fmt.Fprintf(&sb, "• *%s* %s：%s\n", name, m, t.Description)
	}
	b.mu.Lock()
	for _, s := range b.senders {
		fmt.Fprintf(&sb, "  - %s → %s（Slack で追加）\n", s.Host, s.Team)
	}
	b.mu.Unlock()
	sb.WriteString("変えるには：「@noc-bot スイッチとルーターは @l2l3、サーバーは @server」のように書いてください。")
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
