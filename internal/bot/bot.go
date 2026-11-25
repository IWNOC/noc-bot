// Package bot wires the pipeline together:
//
//	lines -> templates + counts -> rules -> incidents -> Slack
//	                     \-> new templates -> laya line desk (teacher: rules, then LLM with tools)
//	every 5 min: window summary -> laya window desk
//	opened incidents -> LLM note (separate from facts) + laya incident desk (team, severity, paging)
//
// Rules never depend on laya or the LLM: if either is down, alerts still go out.
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/IWNOC/noc-bot/internal/config"
	"github.com/IWNOC/noc-bot/internal/drain"
	"github.com/IWNOC/noc-bot/internal/incident"
	"github.com/IWNOC/noc-bot/internal/ingest"
	"github.com/IWNOC/noc-bot/internal/laya"
	"github.com/IWNOC/noc-bot/internal/llm"
	"github.com/IWNOC/noc-bot/internal/notify"
	"github.com/IWNOC/noc-bot/internal/rules"
	"github.com/IWNOC/noc-bot/internal/statebuilder"
	"github.com/IWNOC/noc-bot/internal/stats"
	"github.com/IWNOC/noc-bot/internal/store"
	"github.com/IWNOC/noc-bot/internal/teacher"
)

// Bot is the running pipeline.
type Bot struct {
	Cfg       *config.Config
	Log       *slog.Logger
	Miner     *drain.Miner
	Stats     *stats.Tracker
	Rules     *rules.Engine
	Incidents *incident.Manager
	Laya      *laya.Client // nil: no laya
	LLM       *llm.Client  // nil: no LLM
	Notify    notify.Notifier
	Store     *store.Store
	Known     *teacher.Known

	lineQ    chan rules.Signal
	mu       sync.Mutex
	examples map[string][]string // template id -> recent raw lines
	judged   map[string]bool     // template ids the line desk has decided or that are queued for it
	teams    map[string]config.Team
	senders  []config.Sender // added from Slack; checked before the config file's
	props    map[string]*proposal
	Now      func() time.Time
}

// New builds a bot from its parts.
func New(cfg *config.Config, log *slog.Logger, st *store.Store, ly *laya.Client, lm *llm.Client, nt notify.Notifier) (*Bot, error) {
	tr := stats.NewTracker()
	re, err := rules.New(cfg, tr)
	if err != nil {
		return nil, err
	}
	b := &Bot{Cfg: cfg, Log: log, Miner: drain.New(0.5), Stats: tr, Rules: re, Incidents: incident.NewManager(),
		Laya: ly, LLM: lm, Notify: nt, Store: st, Known: teacher.NewKnown(),
		lineQ: make(chan rules.Signal, 1000), examples: map[string][]string{}, judged: map[string]bool{}, Now: time.Now,
		teams: map[string]config.Team{}, props: map[string]*proposal{}}
	for k, v := range cfg.Teams {
		b.teams[k] = v
	}
	b.loadLearned(context.Background())
	if done, err := st.Subjects(context.Background(), "line"); err == nil {
		b.judged = done
	}
	if snap, err := st.Get(context.Background(), "drain"); err == nil && snap != nil {
		if err := b.Miner.Restore(snap); err != nil {
			log.Warn("drain restore", "err", err)
		} else {
			log.Info("templates restored", "n", b.Miner.Len())
		}
	}
	return b, nil
}

// Handle processes one log line. Cheap: runs on the ingest path.
func (b *Bot) Handle(l ingest.Line) {
	m := b.Miner.Add(l.Program, l.Msg)
	b.Stats.Observe(stats.Key{Kind: "template", ID: m.ID}, l.Recv)
	b.Stats.Observe(stats.Key{Kind: "sender", ID: l.Host}, l.Recv)
	b.mu.Lock()
	ex := append(b.examples[m.ID], l.Raw)
	if len(ex) > 5 {
		ex = ex[len(ex)-5:]
	}
	b.examples[m.ID] = ex
	b.mu.Unlock()
	meta := rules.TemplateMeta{ID: m.ID, Text: m.Template, Program: l.Program, Host: l.Host, From: l.From, Example: l.Msg}
	sig := b.Rules.Observe(meta, m.New, l.Recv, l.Skew())
	if b.Laya == nil {
		return
	}
	// A template learned before laya was reachable (or before a restart) is not new any more, but
	// the line desk has never seen it: send it once.
	b.mu.Lock()
	seen := b.judged[m.ID]
	if !seen {
		b.judged[m.ID] = true
	}
	b.mu.Unlock()
	if seen {
		return
	}
	if sig == nil {
		if b.Rules.Covering(meta, l.Recv) != nil {
			return
		}
		sig = &rules.Signal{Kind: rules.KindNew, Key: "new:" + m.ID, Host: l.Host, From: l.From, Template: m.Template, TplID: m.ID, At: l.Recv}
	}
	select {
	case b.lineQ <- *sig:
	default:
		b.mu.Lock()
		delete(b.judged, m.ID) // try again on a later line
		b.mu.Unlock()
		b.Log.Warn("line desk queue full, will retry", "template", m.ID)
	}
}

// Run starts the background loops and blocks until ctx is done.
func (b *Bot) Run(ctx context.Context, lines <-chan ingest.Line) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case l := <-lines:
				b.Handle(l)
			}
		}
	}()
	go b.lineWorker(ctx)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	n := 0
	for {
		select {
		case <-ctx.Done():
			b.saveTemplates(context.Background())
			return
		case <-tick.C:
			n++
			b.minute(ctx, b.Now())
			if n%5 == 0 && b.Laya != nil {
				go b.window(ctx, b.Now())
			}
			if n%10 == 0 {
				b.saveTemplates(ctx)
			}
		}
	}
}

func (b *Bot) minute(ctx context.Context, now time.Time) {
	b.Stats.Tick(now)
	for _, s := range b.Rules.Evaluate(now) {
		in, opened := b.Incidents.Signal(s)
		if opened {
			go b.open(ctx, in, s)
		}
	}
	for _, ev := range b.Incidents.Tick(now) {
		b.post(ctx, ev)
	}
	b.checkOutcomes(ctx, now)
}

func (b *Bot) saveTemplates(ctx context.Context) {
	if snap, err := b.Miner.Snapshot(); err == nil {
		if err := b.Store.Put(ctx, "drain", snap); err != nil {
			b.Log.Warn("save templates", "err", err)
		}
	}
}

// open enriches a new incident (team, LLM note, laya incident desk) and posts it.
func (b *Bot) open(ctx context.Context, in incident.Incident, s rules.Signal) {
	sender := b.senderFor(s.From, s.Host)
	team := sender.Team
	page := s.Kind == rules.KindSilence || s.Level == stats.LevelStopped // a service going quiet pages
	sev := -1
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	note := ""
	if b.LLM != nil {
		if n, err := b.annotate(ctx, in); err != nil {
			b.Log.Warn("annotate", "incident", in.ID, "err", err)
		} else {
			note = n
		}
	}
	if b.Laya != nil {
		text := b.incidentState(in, sender, note)
		known := map[string]any{}
		if team != "" {
			known["primary_team"] = team
		}
		b.Known.Put(text, known, "")
		if d, err := b.Laya.Decide(ctx, "incident", text); err != nil {
			b.Log.Warn("laya incident desk", "err", err)
		} else {
			b.record(ctx, "incident", in.ID, text, d)
			if a, ok := d.Answers["primary_team"]; ok && team == "" && trusted(a) {
				team = a.String()
			}
			if a, ok := d.Answers["severity"]; ok && trusted(a) {
				if v, ok := a.Int(); ok {
					sev = v
				}
			}
			if a, ok := d.Answers["page_now"]; ok && trusted(a) {
				if v, ok := a.Bool(); ok && v {
					page = true
				}
			}
		}
	}
	in, _ = b.Incidents.Update(in.Key, func(i *incident.Incident) {
		i.Team, i.Severity, i.Page, i.Annotation, i.Notified = team, sev, page, note, b.Now()
	})
	b.post(ctx, incident.Event{Kind: incident.EvOpened, Incident: in})
}

// trusted: an answer we act on. While desks run in shadow mode the served answer is the teacher's
// (rules or the LLM with evidence); a vega answer is used only when laya routes it to auto.
func trusted(a laya.Answer) bool {
	return a.Source == "teacher" || a.Route == "auto"
}

func (b *Bot) post(ctx context.Context, ev incident.Event) {
	mention := ""
	if t, ok := b.team(ev.Incident.Team); ok {
		mention = t.Mention
	}
	text := notify.Render(ev, mention)
	var ts string
	var err error
	if ip, ok := b.Notify.(IncidentPoster); ok && ev.Kind == incident.EvOpened {
		ts, err = ip.PostIncident(ctx, ev.Incident.ID, text, ev.Incident.Thread)
	} else {
		ts, err = b.Notify.Post(ctx, text, ev.Incident.Thread)
	}
	if err != nil {
		b.Log.Error("slack post", "incident", ev.Incident.ID, "err", err)
		return
	}
	if ts != "" && ev.Incident.Thread == "" {
		b.Incidents.Update(ev.Incident.Key, func(i *incident.Incident) { i.Thread = ts })
	}
	if err := b.Store.Incident(ctx, ev.Incident.ID, ev.Incident.Key, ev.Incident.Opened, ev.Incident); err != nil {
		b.Log.Warn("store incident", "err", err)
	}
}

func (b *Bot) record(ctx context.Context, desk, subject, text string, d laya.Decision) int64 {
	id, err := b.Store.Decision(ctx, b.Now(), desk, statebuilder.Version, subject, text, d.Answers, d.TeacherCalled)
	if err != nil {
		b.Log.Warn("store decision", "err", err)
	}
	return id
}

// lineWorker sends new templates to the line desk, one at a time.
func (b *Bot) lineWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case s := <-b.lineQ:
			if b.Laya == nil {
				continue
			}
			b.line(ctx, s)
		}
	}
}

func (b *Bot) line(ctx context.Context, s rules.Signal) {
	now := b.Now()
	meta, _ := b.Rules.Template(s.TplID)
	info, _ := b.Stats.Get(stats.Key{Kind: "template", ID: s.TplID}, now)
	sender := b.senderFor(meta.From, meta.Host)
	known := b.Rules.Covering(meta, now)
	text := statebuilder.LineState(statebuilder.Line{Template: meta.Text, Example: meta.Example, Program: meta.Program,
		Sender: sender, Host: meta.Host, Stats: info, Known: known, Related: b.Incidents.Related("odd:"+s.TplID, now), Now: now})
	exact := map[string]any{}
	if sender.Team != "" {
		exact["team"] = sender.Team
	}
	if known != nil {
		exact["odd"] = false
	}
	b.Known.Put(text, exact, "")
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	d, err := b.Laya.Decide(ctx, "line", text)
	if err != nil {
		b.Log.Warn("laya line desk", "err", err)
		// Ask again later only if laya never answered. A reply we could not read was still decided
		// (and the teacher may already have taught laya from it), so asking again would teach twice.
		if laya.Unreached(err) {
			b.mu.Lock()
			delete(b.judged, s.TplID)
			b.mu.Unlock()
		}
		return
	}
	id := b.record(ctx, "line", s.TplID, text, d)
	odd, ok := d.Answers["odd"]
	if !ok {
		return
	}
	v, ok := odd.Bool()
	if !ok || !v || !trusted(odd) {
		return
	}
	// laya (or its teacher) thinks this pattern is strange: open an incident and check later
	// whether something actually happened on that host.
	b.Store.ExpectOutcome(ctx, id, now.Add(30*time.Minute), "line", meta.Host, text, true)
	sig := rules.Signal{Kind: "odd", Key: "odd:" + s.TplID, Host: meta.Host, From: meta.From, Template: meta.Text, TplID: s.TplID,
		Level: info.Level, At: now, Facts: map[string]string{"example": meta.Example, "frequency": string(info.Level)}}
	if in, opened := b.Incidents.Signal(sig); opened {
		go b.open(context.Background(), in, sig)
	}
}

// window asks the window desk about the last five minutes as a whole.
func (b *Bot) window(ctx context.Context, now time.Time) {
	w := statebuilder.Window{Minutes: 5, Now: now, Known: map[string]uint64{}}
	w.Senders = b.Stats.All("sender", now)
	if len(w.Senders) > 40 {
		w.Senders = w.Senders[:40]
	}
	for _, t := range b.Stats.All("template", now) {
		switch t.Level {
		case stats.LevelNew, stats.LevelAbove, stats.LevelFarAbove, stats.LevelStopped:
			meta, _ := b.Rules.Template(t.Key.ID)
			if b.Rules.Covering(meta, now) != nil {
				continue
			}
			w.Notable = append(w.Notable, statebuilder.WindowItem{Sender: meta.Host, Template: meta.Text, Level: t.Level,
				LastHour: t.LastHour, Last5Min: t.Last5Min, Baseline: t.Baseline, Example: meta.Example})
		}
		if len(w.Notable) >= 40 {
			break
		}
	}
	for id, n := range b.Rules.SuppressedCounts() {
		w.Known[id] = n
	}
	for _, in := range b.Incidents.Open() {
		if in.Kind == rules.KindSilence {
			w.Silent = append(w.Silent, in.Facts["expected"])
		}
	}
	text := statebuilder.WindowState(w)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	d, err := b.Laya.Decide(ctx, "window", text)
	if err != nil {
		b.Log.Warn("laya window desk", "err", err)
		return
	}
	b.record(ctx, "window", now.UTC().Format("2006-01-02T15:04"), text, d)
	a, ok := d.Answers["anything_odd"]
	if v, okb := a.Bool(); !ok || !okb || !v || !trusted(a) {
		return
	}
	where := d.Answers["where"].String()
	sig := rules.Signal{Kind: "window", Key: "window:" + where, Host: where, At: now, Facts: map[string]string{"area": where}}
	if in, opened := b.Incidents.Signal(sig); opened {
		go b.open(context.Background(), in, sig)
	}
}

// checkOutcomes turns what actually happened into labels. Only the positive case is taught: an odd
// pattern followed by another incident on the same host within 30 minutes. Nothing happening is
// not proof that a pattern was harmless, so it teaches nothing.
func (b *Bot) checkOutcomes(ctx context.Context, now time.Time) {
	due, err := b.Store.DueOutcomes(ctx, now)
	if err != nil || len(due) == 0 {
		return
	}
	open := b.Incidents.Open()
	for _, o := range due {
		for _, in := range open {
			if in.Host == o.Subject && in.Kind != "odd" {
				if b.Laya != nil {
					if err := b.Laya.Feedback(ctx, o.Desk, o.Text, map[string]any{"odd": true}, false, laya.OriginOutcome); err != nil {
						b.Log.Warn("outcome feedback", "err", err)
					}
				}
				b.Store.Label(ctx, now, o.Desk, o.Text, map[string]any{"odd": true}, laya.OriginOutcome, "noc-bot", false, "followed by "+in.ID)
				break
			}
		}
		b.Store.OutcomeDone(ctx, o.DecisionID)
	}
}

func (b *Bot) incidentState(in incident.Incident, s config.Sender, note string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Incident of kind %q on %s", in.Kind, in.Host)
	if s.Role != "" {
		fmt.Fprintf(&sb, " (%s)", s.Role)
	}
	sb.WriteString(".\n")
	if in.Template != "" {
		fmt.Fprintf(&sb, "Log pattern: %s\n", in.Template)
	}
	for k, v := range in.Facts {
		fmt.Fprintf(&sb, "%s: %s\n", k, v)
	}
	if rel := b.Incidents.Related(in.Key, b.Now()); len(rel) > 0 {
		fmt.Fprintf(&sb, "Happening at the same time: %s.\n", strings.Join(rel, "; "))
	}
	if note != "" {
		fmt.Fprintf(&sb, "Investigator's note (may be wrong): %s\n", note)
	}
	return sb.String()
}

const annotateSystem = `あなたはイベントネットワークの NOC の一次調査担当です。
与えられた事実と道具の結果だけを根拠に、Slack に載せる「見立て」と「次にやること」を日本語で合計5行以内で書いてください。
- 数字・IP アドレス・時刻は書かない（事実欄にプログラムが正確に書くため）。
- 与えられていない事実を断定しない。根拠が弱いときは「可能性」「要確認」と書く。
- 原因がわからないときは「原因は不明」と書く。`

func (b *Bot) annotate(ctx context.Context, in incident.Incident) (string, error) {
	facts, _ := json.MarshalIndent(in.Facts, "", "  ")
	user := fmt.Sprintf("種類: %s\n送信元: %s\nパターン: %s\n事実:\n%s\n同時に起きていること: %s",
		in.Kind, in.Host, in.Template, facts, strings.Join(b.Incidents.Related(in.Key, b.Now()), "; "))
	out, _, err := b.LLM.Run(ctx, []llm.Message{{Role: "system", Content: annotateSystem}, {Role: "user", Content: user}}, b.Tools(), 3, 400, false)
	return strings.TrimSpace(out), err
}
