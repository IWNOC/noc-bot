// Package incident turns repeated signals into incidents with a lifecycle, so Slack gets one thread
// per problem: a post when it opens, an hourly reminder while it lasts, a note when it ends.
package incident

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/IWNOC/noc-bot/internal/rules"
)

// State of an incident.
type State string

const (
	Open     State = "open"
	Resolved State = "resolved"
)

// Incident groups the signals that share a key.
type Incident struct {
	ID         string
	Key        string
	Kind       string
	Host       string
	Template   string
	TplID      string
	State      State
	Opened     time.Time
	LastSeen   time.Time
	Resolved   time.Time
	Hits       int
	Facts      map[string]string // the latest signal's exact values
	Notified   time.Time         // last post or reminder
	Thread     string            // Slack thread id, set by the notifier
	Team       string
	Severity   int  // 0..3, -1 unknown
	Page       bool // mention the team
	Summary    string
	Annotation string // the investigator's (LLM) note, shown separately from facts
}

// EventKind says what changed.
type EventKind string

const (
	EvOpened   EventKind = "opened"
	EvReminder EventKind = "reminder"
	EvResolved EventKind = "resolved"
)

// Event is something the notifier should post.
type Event struct {
	Kind     EventKind
	Incident Incident // a copy
}

// Manager keeps incidents. Safe for concurrent use.
type Manager struct {
	mu        sync.Mutex
	byKey     map[string]*Incident
	seq       int
	Quiet     time.Duration // no signal this long -> resolved
	Remind    time.Duration // reminder interval while open
	Correlate time.Duration // window for "happening at the same time"
}

// NewManager returns a manager with event-friendly defaults.
func NewManager() *Manager {
	return &Manager{byKey: map[string]*Incident{}, Quiet: 15 * time.Minute, Remind: time.Hour, Correlate: 10 * time.Minute}
}

// Signal folds one rule signal in. It returns the incident (copy) and whether it was just opened.
func (m *Manager) Signal(s rules.Signal) (Incident, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in := m.byKey[s.Key]
	if in == nil || in.State == Resolved {
		m.seq++
		in = &Incident{ID: fmt.Sprintf("%s-%04d", s.At.UTC().Format("0102"), m.seq), Key: s.Key, Kind: s.Kind, Host: s.Host,
			Template: s.Template, TplID: s.TplID, State: Open, Opened: s.At, Severity: -1}
		m.byKey[s.Key] = in
		in.LastSeen, in.Hits, in.Facts = s.At, 1, s.Facts
		return *in, true
	}
	in.LastSeen = s.At
	in.Hits++
	in.Facts = s.Facts
	return *in, false
}

// Update applies f to an incident (team, severity, thread, annotation...).
func (m *Manager) Update(key string, f func(*Incident)) (Incident, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in := m.byKey[key]
	if in == nil {
		return Incident{}, false
	}
	f(in)
	return *in, true
}

// Tick resolves quiet incidents and schedules reminders.
func (m *Manager) Tick(now time.Time) []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Event
	for _, in := range m.byKey {
		if in.State != Open {
			continue
		}
		if now.Sub(in.LastSeen) >= m.Quiet {
			in.State, in.Resolved = Resolved, now
			out = append(out, Event{EvResolved, *in})
			continue
		}
		if !in.Notified.IsZero() && now.Sub(in.Notified) >= m.Remind {
			in.Notified = now
			out = append(out, Event{EvReminder, *in})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Incident.ID < out[j].Incident.ID })
	return out
}

// Related lists other open incidents active within the correlation window, in words.
func (m *Manager) Related(key string, now time.Time) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k, in := range m.byKey {
		if k == key || in.State != Open || now.Sub(in.LastSeen) > m.Correlate {
			continue
		}
		out = append(out, fmt.Sprintf("%s on %s (%s)", in.Kind, in.Host, short(in.Template, 80)))
	}
	sort.Strings(out)
	return out
}

// Open lists open incidents.
func (m *Manager) Open() []Incident {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Incident
	for _, in := range m.byKey {
		if in.State == Open {
			out = append(out, *in)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Opened.Before(out[j].Opened) })
	return out
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ByID finds an incident by id (open or resolved).
func (m *Manager) ByID(id string) (Incident, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, in := range m.byKey {
		if in.ID == id {
			return *in, true
		}
	}
	return Incident{}, false
}

// ByThread finds the incident whose Slack thread is ts.
func (m *Manager) ByThread(ts string) (Incident, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, in := range m.byKey {
		if in.Thread != "" && in.Thread == ts {
			return *in, true
		}
	}
	return Incident{}, false
}
