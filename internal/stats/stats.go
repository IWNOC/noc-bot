// Package stats counts log lines per template and per sender and compares the last hour with what
// is normal, so later stages can say "FAR ABOVE NORMAL" instead of handing laya raw numbers (laya
// reads words far better than it compares numbers).
package stats

import (
	"encoding/json"
	"math"
	"sort"
	"sync"
	"time"
)

// Level describes the last hour against the learned baseline.
type Level string

const (
	LevelNew      Level = "NEW"
	LevelFarAbove Level = "FAR ABOVE NORMAL"
	LevelAbove    Level = "ABOVE NORMAL"
	LevelNormal   Level = "NORMAL"
	LevelBelow    Level = "BELOW NORMAL"
	LevelStopped  Level = "STOPPED"
	LevelLearning Level = "STILL LEARNING WHAT IS NORMAL"
)

const minutes = 60

// Series counts events per minute for the last hour and keeps a slow baseline of the hourly rate.
type Series struct {
	buckets   [minutes]uint32
	cur       int64 // unix minute of buckets[cur%60]
	FirstSeen time.Time
	LastSeen  time.Time
	Total     uint64
	baseline  float64           // events per hour, exponentially smoothed
	baseHours float64           // hours of history behind the baseline
	baseHist  [minutes]float64  // baseline as it was at each of the last 60 minutes
	active    float64           // share of minutes with at least one event (smoothed): how steady it talks
	folded    float64           // events folded into the baseline so far
	hours     [histHours]uint32 // events per clock hour, for "is it still happening / since when"
	curHour   int64             // unix hour of hours[curHour%histHours]
}

const histHours = 72

func (s *Series) roll(now time.Time) {
	m := now.Unix() / 60
	if s.cur == 0 {
		s.cur = m
		return
	}
	for s.cur < m {
		s.cur++
		s.buckets[s.cur%minutes] = 0
	}
}

func (s *Series) rollHour(now time.Time) {
	h := now.Unix() / 3600
	if s.curHour == 0 || h-s.curHour >= histHours {
		if s.curHour != 0 {
			s.hours = [histHours]uint32{}
		}
		s.curHour = h
		return
	}
	for s.curHour < h {
		s.curHour++
		s.hours[s.curHour%histHours] = 0
	}
}

// Hourly returns the counts of the last n clock hours, oldest first, with the hour each starts at.
func (s *Series) Hourly(now time.Time, n int) ([]time.Time, []uint32) {
	if n > histHours {
		n = histHours
	}
	h := now.Unix() / 3600
	var ts []time.Time
	var cs []uint32
	for i := n - 1; i >= 0; i-- {
		x := h - int64(i)
		var c uint32
		if s.curHour != 0 && x <= s.curHour && s.curHour-x < histHours {
			c = s.hours[x%histHours]
		}
		ts = append(ts, time.Unix(x*3600, 0))
		cs = append(cs, c)
	}
	return ts, cs
}

func (s *Series) add(now time.Time) {
	s.roll(now)
	s.rollHour(now)
	s.hours[s.curHour%histHours]++
	s.buckets[s.cur%minutes]++
	if s.FirstSeen.IsZero() {
		s.FirstSeen = now
	}
	s.LastSeen = now
	s.Total++
}

// LastHour is the number of events in the last 60 minutes.
func (s *Series) LastHour() uint64 {
	var n uint64
	for _, b := range s.buckets {
		n += uint64(b)
	}
	return n
}

// LastMinutes counts the last k minutes (k <= 60).
func (s *Series) LastMinutes(k int) uint64 {
	var n uint64
	for i := 0; i < k && i < minutes; i++ {
		n += uint64(s.buckets[(s.cur-int64(i))%minutes+0])
	}
	return n
}

// ActiveMinutes is how many of the last 60 minutes had at least one event. A periodic job
// (logrotate, a cron task) bursts in one or two minutes; a real problem keeps going.
func (s *Series) ActiveMinutes() int {
	n := 0
	for _, b := range s.buckets {
		if b > 0 {
			n++
		}
	}
	return n
}

// Baseline is the smoothed normal hourly rate and how many hours it is built on.
func (s *Series) Baseline() (perHour float64, hours float64) { return s.baseline, s.baseHours }

// updateBaseline folds one minute of history into the baseline. Half-life: 6 hours.
func (s *Series) updateBaseline() {
	const halfLifeMin = 6 * 60
	alpha := 1 - math.Pow(0.5, 1.0/halfLifeMin)
	n := float64(s.buckets[(s.cur-1+minutes)%minutes]) // last full minute
	s.folded += n
	s.baseHours += 1.0 / 60
	if s.baseHours < 6 {
		// the first hours: the plain average so far. Seeding from the first minute made one early
		// line look like "60 per hour" for half a day, hiding real rises.
		s.baseline = s.folded / s.baseHours
	} else {
		s.baseline += alpha * (n*60 - s.baseline)
	}
	s.baseHist[s.cur%minutes] = s.baseline
	const activeHalfLifeMin = 120
	beta := 1 - math.Pow(0.5, 1.0/activeHalfLifeMin)
	on := 0.0
	if s.buckets[(s.cur-1+minutes)%minutes] > 0 {
		on = 1
	}
	if s.baseHours <= 1.0/60 {
		s.active = on
	} else {
		s.active += beta * (on - s.active)
	}
}

// priorBaseline is the baseline as it stood an hour ago, so a burst in the last hour does not
// raise its own yardstick.
func (s *Series) priorBaseline() float64 {
	if s.baseHours < 2 {
		return s.baseline
	}
	return s.baseHist[(s.cur+1)%minutes]
}

// Level compares the last hour with the baseline.
func (s *Series) Level(now time.Time) Level {
	if now.Sub(s.FirstSeen) < time.Hour {
		if s.Total <= 1 || now.Sub(s.FirstSeen) < 10*time.Minute {
			return LevelNew
		}
		return LevelLearning
	}
	if s.baseHours < 1 {
		return LevelLearning
	}
	cur := float64(s.LastHour())
	base := s.priorBaseline()
	// STOPPED means "something that talks nearly every minute has said nothing for 15 minutes".
	// Senders that log in bursts (an NTP server, logrotate) are never STOPPED, however busy a burst was.
	if s.active >= 0.8 && s.baseHours >= 2 && s.LastMinutes(15) == 0 {
		return LevelStopped
	}
	switch {
	case cur >= 5 && cur >= 10*math.Max(base, 0.5):
		return LevelFarAbove
	case cur >= 3 && cur >= 3*math.Max(base, 0.5):
		return LevelAbove
	case base >= 3 && cur <= base/3:
		return LevelBelow
	default:
		return LevelNormal
	}
}

// Key identifies a series.
type Key struct {
	Kind string // "template" or "sender"
	ID   string
}

// Info describes one series for callers.
type Info struct {
	Key       Key
	Level     Level
	LastHour  uint64
	Last5Min  uint64
	ActiveMin int // minutes of the last hour with events
	Baseline  float64
	FirstSeen time.Time
	LastSeen  time.Time
	Total     uint64
}

// Tracker holds all series. Safe for concurrent use.
type Tracker struct {
	mu       sync.Mutex
	series   map[Key]*Series
	lastTick int64
	now      func() time.Time
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker {
	return &Tracker{series: map[Key]*Series{}, now: time.Now}
}

// Observe counts one event at time t (the syslog server's receive time).
func (t *Tracker) Observe(k Key, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.series[k]
	if s == nil {
		s = &Series{}
		t.series[k] = s
	}
	s.add(at)
}

// Tick rolls every series to `now` and folds finished minutes into the baselines. Call once a minute.
func (t *Tracker) Tick(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m := now.Unix() / 60
	if t.lastTick == m {
		return
	}
	t.lastTick = m
	for _, s := range t.series {
		s.roll(now)
		s.updateBaseline()
	}
}

// Get returns one series' info.
func (t *Tracker) Get(k Key, now time.Time) (Info, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.series[k]
	if s == nil {
		return Info{}, false
	}
	return info(k, s, now), true
}

func info(k Key, s *Series, now time.Time) Info {
	b := s.priorBaseline()
	return Info{Key: k, Level: s.Level(now), LastHour: s.LastHour(), Last5Min: s.LastMinutes(5), ActiveMin: s.ActiveMinutes(),
		Baseline: b, FirstSeen: s.FirstSeen, LastSeen: s.LastSeen, Total: s.Total}
}

// All returns every series of a kind, busiest first.
func (t *Tracker) All(kind string, now time.Time) []Info {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Info
	for k, s := range t.series {
		if k.Kind == kind {
			out = append(out, info(k, s, now))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastHour > out[j].LastHour })
	return out
}

// Hourly returns one series' hourly history (see Series.Hourly).
func (t *Tracker) Hourly(k Key, now time.Time, n int) ([]time.Time, []uint32, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.series[k]
	if s == nil {
		return nil, nil, false
	}
	ts, cs := s.Hourly(now, n)
	return ts, cs, true
}

// saved is the on-disk form of a series.
type saved struct {
	Key       Key
	Buckets   [minutes]uint32
	Cur       int64
	FirstSeen time.Time
	LastSeen  time.Time
	Total     uint64
	Baseline  float64
	BaseHours float64
	BaseHist  [minutes]float64
	Active    float64
	Folded    float64
	Hours     [histHours]uint32
	CurHour   int64
}

// Snapshot serialises every series, so baselines and history survive a restart. (Without it every
// deploy made all patterns "new" again and blinded the rate rules for hours.)
func (t *Tracker) Snapshot() ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]saved, 0, len(t.series))
	for k, s := range t.series {
		out = append(out, saved{k, s.buckets, s.cur, s.FirstSeen, s.LastSeen, s.Total, s.baseline, s.baseHours, s.baseHist, s.active, s.folded, s.hours, s.curHour})
	}
	return json.Marshal(out)
}

// Restore loads a snapshot.
func (t *Tracker) Restore(b []byte) error {
	var in []saved
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, x := range in {
		t.series[x.Key] = &Series{buckets: x.Buckets, cur: x.Cur, FirstSeen: x.FirstSeen, LastSeen: x.LastSeen, Total: x.Total,
			baseline: x.Baseline, baseHours: x.BaseHours, baseHist: x.BaseHist, active: x.Active, folded: x.Folded, hours: x.Hours, curHour: x.CurHour}
	}
	return nil
}
