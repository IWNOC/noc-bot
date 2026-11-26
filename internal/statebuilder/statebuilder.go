// Package statebuilder writes the text laya reads. Tested on the event's own logs: laya judged
// plain English with frequencies as words ("FAR ABOVE NORMAL") far better than terse key=value
// records with raw numbers, which it confidently got wrong. Change the wording only together with a
// re-run of the evaluation set; bump Version when you do.
package statebuilder

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/IWNOC/noc-bot/internal/config"
	"github.com/IWNOC/noc-bot/internal/stats"
)

// Version of the wording. Stored with every decision so evaluations compare like with like.
const Version = "line-v3" // v3: no "happening at the same time" (it leaked noisy hosts' context)

var jst = time.FixedZone("JST", 9*3600)

// Line describes one log pattern.
type Line struct {
	Template string
	Example  string
	Program  string
	Sender   config.Sender
	Host     string
	Stats    stats.Info
	Known    *config.KnownIssue
	Related  []string // other things going on at the same time, already in words
	Notes    []string // what operators told noc-bot about this sender (its memory)
	Now      time.Time
}

// LineState renders a Line for laya's line desk.
func LineState(l Line) string {
	var b strings.Builder
	who := senderWords(l.Sender, l.Host)
	fmt.Fprintf(&b, "Log pattern from %s, program %q:\n  %s\n", who, l.Program, l.Template)
	if l.Example != "" {
		fmt.Fprintf(&b, "Example line: %s\n", clip(l.Example, 400))
	}
	fmt.Fprintf(&b, "Frequency: %s.\n", frequencyWords(l.Stats))
	if !l.Stats.FirstSeen.IsZero() {
		fmt.Fprintf(&b, "First seen %s; last seen %s.\n", ago(l.Now, l.Stats.FirstSeen), ago(l.Now, l.Stats.LastSeen))
	}
	if l.Known != nil {
		fmt.Fprintf(&b, "Known issue: yes. %s (accepted, will not be fixed during the event).\n", l.Known.Reason)
	} else {
		b.WriteString("Known issue: none.\n")
	}
	for _, n := range l.Notes {
		fmt.Fprintf(&b, "Operators told us: %s\n", n)
	}
	fmt.Fprintf(&b, "Time of day: %s.\n", timeOfDay(l.Now))
	return b.String()
}

func senderWords(s config.Sender, host string) string {
	name := s.Name
	if name == "" {
		name = host
	}
	parts := []string{}
	if s.Role != "" {
		parts = append(parts, s.Role)
	} else {
		parts = append(parts, "device")
	}
	parts = append(parts, name)
	out := strings.Join(parts, " ")
	if s.Vendor != "" {
		out += " (" + s.Vendor + ")"
	}
	return out
}

func frequencyWords(i stats.Info) string {
	switch i.Level {
	case stats.LevelNew:
		return fmt.Sprintf("NEW, never seen before this hour (%d so far)", i.Total)
	case stats.LevelLearning:
		return fmt.Sprintf("%s (%d in the last hour)", i.Level, i.LastHour)
	case stats.LevelStopped:
		return fmt.Sprintf("STOPPED (none in the last hour, normally about %s per hour)", rate(i.Baseline))
	}
	return fmt.Sprintf("%s (%d in the last hour, normally %s per hour)", i.Level, i.LastHour, rate(i.Baseline))
}

func rate(r float64) string {
	switch {
	case r < 1:
		return "under 1"
	case r < 10:
		return fmt.Sprintf("about %.0f", r)
	default:
		return fmt.Sprintf("about %d", int(math.Round(r/5)*5))
	}
}

func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 2*time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

func timeOfDay(t time.Time) string {
	h := t.In(jst).Hour()
	switch {
	case h < 6:
		return "night (few participants online)"
	case h < 9:
		return "early morning"
	case h < 18:
		return "daytime, event sessions running"
	case h < 22:
		return "evening"
	default:
		return "night (few participants online)"
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// WindowItem is one notable thing in a summary window.
type WindowItem struct {
	Sender   string
	Template string
	Level    stats.Level
	LastHour uint64
	Last5Min uint64
	Baseline float64
	Example  string
}

// Window summarises a few minutes of everything for laya's window desk (read with its long context).
type Window struct {
	Minutes int
	Senders []stats.Info // per sender
	Notable []WindowItem // new, above, far above, stopped templates
	Silent  []string
	Known   map[string]uint64 // known issue -> lines covered
	Now     time.Time
}

// WindowState renders a Window.
func WindowState(w Window) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Summary of the last %d minutes received by the event's syslog server (%s).\n", w.Minutes, timeOfDay(w.Now))
	b.WriteString("Senders (lines in the last 5 minutes, and the last hour against normal):\n")
	for _, s := range w.Senders {
		fmt.Fprintf(&b, "- %s: %d in 5 min; last hour %s\n", s.Key.ID, s.Last5Min, frequencyWords(s))
	}
	if len(w.Silent) > 0 {
		fmt.Fprintf(&b, "Expected senders that went silent: %s.\n", strings.Join(w.Silent, ", "))
	}
	if len(w.Notable) == 0 {
		b.WriteString("No log pattern is new or away from its normal rate.\n")
	} else {
		b.WriteString("Log patterns that are new or away from their normal rate:\n")
		sort.SliceStable(w.Notable, func(i, j int) bool { return rank(w.Notable[i].Level) < rank(w.Notable[j].Level) })
		for _, n := range w.Notable {
			fmt.Fprintf(&b, "- [%s] from %s: %s\n  e.g. %s\n", n.Level, n.Sender, n.Template, clip(n.Example, 240))
		}
	}
	if len(w.Known) > 0 {
		ids := make([]string, 0, len(w.Known))
		for id := range w.Known {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		b.WriteString("Known, accepted issues still occurring (not problems by themselves): ")
		for i, id := range ids {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s (%d)", id, w.Known[id])
		}
		b.WriteString(".\n")
	}
	return b.String()
}

func rank(l stats.Level) int {
	switch l {
	case stats.LevelStopped:
		return 0
	case stats.LevelFarAbove:
		return 1
	case stats.LevelNew:
		return 2
	case stats.LevelAbove:
		return 3
	}
	return 4
}
