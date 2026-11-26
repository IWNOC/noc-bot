// Package rules turns counts and clocks into signals with fixed, explainable rules. Rules run before
// any model and keep working when laya or the LLM is down.
package rules

import (
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/IWNOC/noc-bot/internal/config"
	"github.com/IWNOC/noc-bot/internal/stats"
)

// Kind of signal.
const (
	KindRate      = "rate"       // a template is FAR ABOVE NORMAL or STOPPED
	KindSilence   = "silence"    // an expected sender went quiet
	KindClockSkew = "clock_skew" // a sender's clock is off, and no known issue covers it
	KindNew       = "new"        // a template never seen before (goes to the line desk, not straight to Slack)
)

// Signal is one rule hit.
type Signal struct {
	Kind     string
	Key      string // stable identity for de-duplication, e.g. "rate:<template id>"
	Host     string
	From     string
	Template string
	TplID    string
	Level    stats.Level
	Facts    map[string]string // exact values, shown verbatim in Slack
	At       time.Time
}

// TemplateMeta is what the engine remembers about each template.
type TemplateMeta struct {
	ID, Text, Program, Host, From, Example string
}

// Engine evaluates rules. Safe for concurrent use.
type Engine struct {
	cfg *config.Config
	tr  *stats.Tracker

	mu         sync.Mutex
	tpl        map[string]TemplateMeta
	hostSeen   map[string]time.Time
	skew       map[string]time.Duration // smoothed skew per host
	known      []compiledKnown
	Suppressed map[string]uint64 // known issue id -> lines covered; read with SuppressedCounts
	started    time.Time         // first Evaluate: "never seen" waits a full silence period after this
}

type compiledKnown struct {
	config.KnownIssue
	re *regexp.Regexp
}

// New builds an engine. Known issues come from config plus whatever the store adds later (AddKnown).
func New(cfg *config.Config, tr *stats.Tracker) (*Engine, error) {
	e := &Engine{cfg: cfg, tr: tr, tpl: map[string]TemplateMeta{}, hostSeen: map[string]time.Time{},
		skew: map[string]time.Duration{}, Suppressed: map[string]uint64{}}
	for _, k := range cfg.Known {
		if err := e.AddKnown(k); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// AddKnown registers a known issue (from config or confirmed in Slack).
func (e *Engine) AddKnown(k config.KnownIssue) error {
	var re *regexp.Regexp
	if k.Template != "" {
		var err error
		if re, err = regexp.Compile(k.Template); err != nil {
			return fmt.Errorf("known issue %s: %w", k.ID, err)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, c := range e.known {
		if c.ID == k.ID {
			e.known[i] = compiledKnown{k, re}
			return nil
		}
	}
	e.known = append(e.known, compiledKnown{k, re})
	return nil
}

// Known lists active known issues.
func (e *Engine) Known(now time.Time) []config.KnownIssue {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []config.KnownIssue
	for _, k := range e.known {
		if k.Expires.IsZero() || now.Before(k.Expires) {
			out = append(out, k.KnownIssue)
		}
	}
	return out
}

// Observe records one line's template, sender and clock. It returns a KindNew signal the first time
// a template appears.
func (e *Engine) Observe(m TemplateMeta, isNew bool, recv time.Time, skew time.Duration) *Signal {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.tpl[m.ID]; !ok || isNew {
		e.tpl[m.ID] = m
	}
	e.hostSeen[m.Host] = recv
	if skew != 0 {
		old, ok := e.skew[m.Host]
		if !ok {
			e.skew[m.Host] = skew
		} else {
			e.skew[m.Host] = old + (skew-old)/10
		}
	}
	if !isNew {
		return nil
	}
	s := &Signal{Kind: KindNew, Key: "new:" + m.ID, Host: m.Host, From: m.From, Template: m.Text, TplID: m.ID, Level: stats.LevelNew, At: recv,
		Facts: map[string]string{"example": m.Example}}
	if k := e.coveringLocked(m, recv); k != nil {
		e.Suppressed[k.ID]++
		return nil
	}
	return s
}

// Covering returns the known issue covering a template, if any.
func (e *Engine) Covering(m TemplateMeta, now time.Time) *config.KnownIssue {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.coveringLocked(m, now)
}

func (e *Engine) coveringLocked(m TemplateMeta, now time.Time) *config.KnownIssue {
	for _, k := range e.known {
		if !k.Expires.IsZero() && now.After(k.Expires) {
			continue
		}
		if k.re == nil || k.ExpectSkew != 0 {
			continue // clock-skew issues do not cover templates
		}
		if k.Program != "" && k.Program != m.Program {
			continue
		}
		if k.Host != "" && !config.Glob(k.Host, m.Host) {
			continue
		}
		if k.Vendor != "" && e.cfg.SenderFor(m.From, m.Host).Vendor != k.Vendor {
			continue
		}
		if k.re.MatchString(m.Text) {
			kk := k.KnownIssue
			return &kk
		}
	}
	return nil
}

// Evaluate runs the periodic rules. Call once a minute after stats.Tracker.Tick.
func (e *Engine) Evaluate(now time.Time) []Signal {
	var out []Signal
	for _, in := range e.tr.All("template", now) {
		// a log pattern going quiet is usually the problem ending (or a periodic job between runs);
		// only senders going quiet are alerts
		if in.Level != stats.LevelFarAbove {
			continue
		}
		// a burst within a few minutes is a job running (logrotate, a reload), not a problem: an
		// operator ignores it. Only a rise that keeps going over several minutes is an alert.
		if in.ActiveMin < 5 {
			continue
		}
		e.mu.Lock()
		m := e.tpl[in.Key.ID]
		k := e.coveringLocked(m, now)
		if k != nil {
			e.Suppressed[k.ID] += in.Last5Min
		}
		e.mu.Unlock()
		if k != nil {
			continue
		}
		out = append(out, Signal{Kind: KindRate, Key: "rate:" + in.Key.ID, Host: m.Host, From: m.From, Template: m.Text, TplID: m.ID,
			Level: in.Level, At: now, Facts: map[string]string{
				"last_hour": fmt.Sprint(in.LastHour), "normal_per_hour": fmt.Sprintf("%.1f", in.Baseline), "active_minutes": fmt.Sprint(in.ActiveMin),
				"first_seen": in.FirstSeen.UTC().Format(time.RFC3339), "example": m.Example}})
	}
	// any sender that normally talks a lot and went quiet: no list needed, the baseline says it
	for _, in := range e.tr.All("sender", now) {
		if in.Level == stats.LevelStopped {
			out = append(out, Signal{Kind: KindSilence, Key: "silence:" + in.Key.ID, Host: in.Key.ID, Level: stats.LevelStopped, At: now,
				Facts: map[string]string{"normal_per_hour": fmt.Sprintf("%.0f", in.Baseline), "last_seen": in.LastSeen.UTC().Format(time.RFC3339)}})
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started.IsZero() {
		e.started = now
	}
	for _, x := range e.cfg.Expected {
		var last time.Time
		var host string
		for h, t := range e.hostSeen {
			if config.Glob(x.Host, h) && t.After(last) {
				last, host = t, h
			}
		}
		if last.IsZero() && now.Sub(e.started) <= x.Silence {
			continue // just started: give every sender a full silence period to show up
		}
		if host == "" {
			host = x.Host
		}
		if last.IsZero() || now.Sub(last) > x.Silence {
			facts := map[string]string{"expected": x.Host, "silent_for": "never seen since noc-bot started"}
			if !last.IsZero() {
				facts["silent_for"] = now.Sub(last).Round(time.Minute).String()
				facts["last_seen"] = last.UTC().Format(time.RFC3339)
			}
			out = append(out, Signal{Kind: KindSilence, Key: "silence:" + x.Host, Host: host, Level: stats.LevelStopped, At: now, Facts: facts})
		}
	}
	hosts := make([]string, 0, len(e.skew))
	for h := range e.skew {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		sk := e.skew[h]
		if abs(sk) <= e.cfg.Clock.MaxSkew {
			continue
		}
		if k := e.skewCoverLocked(h, sk, now); k != nil {
			e.Suppressed[k.ID]++
			continue
		}
		out = append(out, Signal{Kind: KindClockSkew, Key: "skew:" + h, Host: h, At: now, Facts: map[string]string{
			"skew": sk.Round(time.Second).String()}})
	}
	return out
}

// skewCoverLocked: a known clock issue covers this host if the skew is within its expected band.
// A skew outside the band (the clock drifted further, or someone fixed it) is reported.
func (e *Engine) skewCoverLocked(host string, sk time.Duration, now time.Time) *config.KnownIssue {
	for _, k := range e.known {
		if k.ExpectSkew == 0 || (!k.Expires.IsZero() && now.After(k.Expires)) {
			continue
		}
		if k.Host != "" && !config.Glob(k.Host, host) {
			continue
		}
		if k.Vendor != "" && e.vendorOfHostLocked(host) != k.Vendor {
			continue
		}
		if abs(sk-k.ExpectSkew) <= k.SkewTolerance {
			kk := k.KnownIssue
			return &kk
		}
	}
	return nil
}

func (e *Engine) vendorOfHostLocked(host string) string {
	for _, m := range e.tpl {
		if m.Host == host {
			return e.cfg.SenderFor(m.From, host).Vendor
		}
	}
	return e.cfg.SenderFor("", host).Vendor
}

// Template returns what is known about a template.
func (e *Engine) Template(id string) (TemplateMeta, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	m, ok := e.tpl[id]
	return m, ok
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// SuppressedCounts returns how many lines each known issue has covered so far.
func (e *Engine) SuppressedCounts() map[string]uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]uint64, len(e.Suppressed))
	for k, v := range e.Suppressed {
		out[k] = v
	}
	return out
}
