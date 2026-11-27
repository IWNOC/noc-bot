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
	"github.com/IWNOC/noc-bot/internal/web"
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
	Agent     *llm.Client  // conversation model; nil: use LLM
	Web       *web.Client  // internet lookups for the conversation agent; nil: none
	Notify    notify.Notifier
	Store     *store.Store
	Known     *teacher.Known

	lineQ    chan rules.Signal
	mu       sync.Mutex
	examples map[string][]string // template id -> recent raw lines
	judged   map[string]bool     // template ids the line desk has decided or that are queued for it
	seenPid  map[string]bool     // patterns (template@host) seen since start
	started  time.Time
	heldSeen map[string]bool // held batches already shown to people
	teams    map[string]config.Team
	senders  []config.Sender // added from Slack; checked before the config file's
	props    map[string]*proposal
	llmSlots chan struct{}
	tierMu   sync.Mutex
	nowPosts []time.Time
	stormQ   []incident.Incident
	configAt map[string]time.Time
	lastDig  time.Time
	inv      invState
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
		lineQ: make(chan rules.Signal, 1000), examples: map[string][]string{}, judged: map[string]bool{}, seenPid: map[string]bool{}, heldSeen: map[string]bool{}, started: time.Now(), Now: time.Now,
		teams: map[string]config.Team{}, props: map[string]*proposal{}, llmSlots: make(chan struct{}, 2), configAt: map[string]time.Time{}}
	for k, v := range cfg.Teams {
		b.teams[k] = v
	}
	b.loadLearned(context.Background())
	if done, err := st.Subjects(context.Background(), "line", statebuilder.Version); err == nil {
		b.judged = done
	}
	if snap, err := st.Get(context.Background(), "stats"); err == nil && snap != nil {
		if err := b.Stats.Restore(snap); err != nil {
			log.Warn("stats restore", "err", err)
		}
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
	// A pattern belongs to the host that sent it. Sharing one id across hosts attributed five
	// switches' lines to whichever host was seen first (the investigator caught this).
	pid := m.ID + "@" + l.Host
	m.ID = pid
	drainNew := m.New
	b.Stats.Observe(stats.Key{Kind: "template", ID: pid}, l.Recv)
	b.Stats.Observe(stats.Key{Kind: "sender", ID: l.Host}, l.Recv)
	b.mu.Lock()
	m.New = !b.seenPid[pid]
	b.seenPid[pid] = true
	// lines are kept with the time we received them, in JST: senders' own clocks are not trusted
	// (some send JST labelled as UTC), and two clocks in one line confused every reader
	ex := append(b.examples[pid], fmt.Sprintf("%s %s %s: %s", l.Recv.In(jst).Format("01/02 15:04:05 JST"), l.Host, l.Program, l.Msg))
	if len(ex) > 8 {
		ex = ex[len(ex)-8:]
	}
	b.examples[pid] = ex
	b.mu.Unlock()
	b.noteConfigChange(l.Host, l.Msg, l.Recv)
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
	// Soon after a start, patterns that are only new to this process (not to the miner) are a
	// backlog being judged again: learn from them, but do not alert on old news.
	if !drainNew && b.Now().Sub(b.started) < 2*time.Hour {
		if sig.Facts == nil {
			sig.Facts = map[string]string{}
		}
		sig.Facts["backfill"] = "1"
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
			// the window desk is no longer asked: it never opened useful alerts and cost a teacher
			// call every five minutes; the 30-minute digest covers "how does the whole look"

			if n%10 == 0 {
				b.saveTemplates(ctx)
			}
			if n == 3 || n%int(patrolEvery/time.Minute) == 0 { // first patrol soon after start
				go b.patrol(ctx, b.Now())
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
		if ev.Incident.Tier == TierNow { // reminders and "resolved" only for what was posted on its own
			b.post(ctx, ev)
		}
	}
	b.flushStorm(ctx)
	b.judgeAttention(ctx, now)
	if now.Sub(b.lastDig) >= digestInterval {
		if !b.lastDig.IsZero() {
			b.digest(ctx, now)
			go b.checkHeld(ctx)
		}
		b.lastDig = now
	}
	// outcome labels are paused: "another incident on the same host" was true on every noisy host,
	// so it taught laya that noise is odd. Restart only with person-confirmed incidents.
}

func (b *Bot) saveTemplates(ctx context.Context) {
	if snap, err := b.Stats.Snapshot(); err == nil {
		if err := b.Store.Put(ctx, "stats", snap); err != nil {
			b.Log.Warn("save stats", "err", err)
		}
	}
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
	tier, why, page := b.decideTier(ctx, in, s)

	// laya's incident desk still answers (it is learning severity, area and cause), but its
	// answers only fill in the team and the severity shown; they never decide who is paged.
	if b.Laya != nil {
		stateText := b.incidentState(in, sender, "")
		b.Known.Put(stateText, "", map[string]any{}, "")
		go func() {
			lctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			dec, err := b.Laya.Decide(lctx, "incident", stateText)
			if err != nil {
				b.Log.Warn("laya incident desk", "incident", in.ID, "err", err)
				return
			}
			b.record(context.Background(), "incident", in.ID, stateText, dec)
			b.Incidents.Update(in.Key, func(i *incident.Incident) {
				if a, ok := dec.Answers["severity"]; ok && trusted(a) {
					if v, ok := a.Int(); ok {
						i.Severity = v
					}
				}
				if a, ok := dec.Answers["area"]; ok && i.Team == "" && trusted(a) {
					i.Team = b.teamForArea(a.String())
				}
			})
		}()
	}
	in, _ = b.Incidents.Update(in.Key, func(i *incident.Incident) {
		i.Tier, i.Why, i.Page, i.Team, i.Severity = tier, why, page, team, -1
	})
	if tier != TierNow {
		b.Store.Incident(context.Background(), in.ID, in.Key, in.Opened, in) // digest / silent: kept, not posted now
		return
	}
	if prev, why := b.relatedThread(in); prev != "" {
		// a recurrence, or the same host's story continuing: add to that thread, not a new post
		in, _ = b.Incidents.Update(in.Key, func(i *incident.Incident) { i.Thread = prev; i.Why = why })
	} else if b.stormy(b.Now()) {
		b.tierMu.Lock()
		b.stormQ = append(b.stormQ, in)
		b.tierMu.Unlock()
		return
	}
	if b.acquireLLM(ctx, 20*time.Second) {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		title, body, _, ok := b.compose(cctx, in)
		cancel()
		b.releaseLLM()
		if ok {
			in, _ = b.Incidents.Update(in.Key, func(i *incident.Incident) { i.Title, i.Body = title, body })
		}
	}
	in, _ = b.Incidents.Update(in.Key, func(i *incident.Incident) { i.Notified = b.Now() })
	pctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b.post(pctx, incident.Event{Kind: incident.EvOpened, Incident: in})
}

// LLM slots: at most two alerts are written at once, so a burst of alerts does not pile up on the
// local LLM until every one times out.
func (b *Bot) acquireLLM(ctx context.Context, wait time.Duration) bool {
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case b.llmSlots <- struct{}{}:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

func (b *Bot) releaseLLM() { <-b.llmSlots }

// trusted: an answer we act on. While desks run in shadow mode the served answer is the teacher's
// (rules or the LLM with evidence); a vega answer is used only when laya routes it to auto.
func trusted(a laya.Answer) bool {
	// the teacher's answer, or laya's own where the Escalator let it stand (it earned trust on that
	// question and was not sent to the teacher). A laya answer that was escalated but got no
	// teacher reply is not acted on.
	return a.Source == "teacher" || a.Route == "auto" || (a.Source == "vega" && a.Escalated == "")
}

func (b *Bot) post(ctx context.Context, ev incident.Event) {
	mention := ""
	if t, ok := b.team(ev.Incident.Team); ok {
		mention = t.Mention
	}
	text := notify.Render(ev, mention)
	if ev.Kind == incident.EvOpened && ev.Incident.Title != "" {
		text = renderComposed(ev, ev.Incident.Title, ev.Incident.Body, mention)
	}
	var ts string
	var err error
	if ip, ok := b.Notify.(IncidentPoster); ok && ev.Kind == incident.EvOpened {
		ts, err = ip.PostIncident(ctx, ev.Incident.ID, text, ev.Incident.Thread)
	} else {
		ts, err = b.Notify.Post(ctx, text, ev.Incident.Thread)
	}
	if err != nil {
		b.Log.Warn("slack post failed, retrying", "incident", ev.Incident.ID, "err", err)
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if ip, ok := b.Notify.(IncidentPoster); ok && ev.Kind == incident.EvOpened {
			ts, err = ip.PostIncident(rctx, ev.Incident.ID, text, ev.Incident.Thread)
		} else {
			ts, err = b.Notify.Post(rctx, text, ev.Incident.Thread)
		}
		if err != nil {
			b.Log.Error("slack post", "incident", ev.Incident.ID, "err", err)
			return
		}
	}
	if ts != "" && ev.Incident.Thread == "" {
		b.Incidents.Update(ev.Incident.Key, func(i *incident.Incident) { i.Thread = ts })
		if lp, ok := b.Notify.(LogPoster); ok && ev.Kind == incident.EvOpened {
			b.mu.Lock()
			logs := strings.Join(b.examples[ev.Incident.TplID], "\n")
			b.mu.Unlock()
			if logs != "" {
				if err := lp.PostLogs(ctx, ts, "ログ（"+ev.Incident.Host+"）", logs); err != nil {
					b.Log.Warn("post logs", "err", err)
				}
			}
		}
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
	// what the investigator has found out about this pattern so far (it decides what to look up)
	mean, ents := b.background(ctx, s.TplID, meta.Example)
	text := statebuilder.LineState(statebuilder.Line{Template: meta.Text, Example: meta.Example, Program: meta.Program, Meaning: mean, Entities: ents,
		Sender: sender, Host: meta.Host, Stats: info, Known: known, Related: b.Incidents.Related("odd:"+s.TplID, now),
		Notes: b.memoryAbout(ctx, meta.Host), Now: now})
	exact := map[string]any{}
	if known != nil {
		exact["odd"] = false
	}
	b.Known.Put(text, s.TplID, exact, "")
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
	if !ok || !v || !trusted(odd) || s.Facts["backfill"] == "1" {
		return
	}
	// laya (or its teacher) thinks this pattern is strange: open an incident and check later
	// whether something actually happened on that host.
	_ = id
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
	// The window desk's verdicts are kept as data but no longer open alerts: it called 92% of
	// windows odd. The 30-minute digest covers "how does the whole look".
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
	for _, m := range b.memoryAbout(context.Background(), in.Host) {
		fmt.Fprintf(&sb, "Operators told us: %s\n", m)
	}
	mean, ents := b.background(context.Background(), in.TplID, in.Facts["example"])
	if mean != "" {
		fmt.Fprintf(&sb, "What this message means (looked up, may be wrong): %s\n", mean)
	}
	for _, e := range ents {
		fmt.Fprintf(&sb, "Outside party: %s\n", e)
	}
	if note != "" {
		fmt.Fprintf(&sb, "Investigator's note (may be wrong): %s\n", note)
	}
	return sb.String()
}
