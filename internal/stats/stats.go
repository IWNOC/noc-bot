// Package stats counts log lines per template and per sender and compares the last hour with what
// is normal, so later stages can say "FAR ABOVE NORMAL" instead of handing laya raw numbers (laya
// reads words far better than it compares numbers).
package stats

import (
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
	baseline  float64          // events per hour, exponentially smoothed
	baseHours float64          // hours of history behind the baseline
	baseHist  [minutes]float64 // baseline as it was at each of the last 60 minutes
	active    float64          // share of minutes with at least one event (smoothed): how steady it talks
}

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

func (s *Series) add(now time.Time) {
	s.roll(now)
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

// Baseline is the smoothed normal hourly rate and how many hours it is built on.
func (s *Series) Baseline() (perHour float64, hours float64) { return s.baseline, s.baseHours }

// updateBaseline folds one minute of history into the baseline. Half-life: 6 hours.
func (s *Series) updateBaseline() {
	const halfLifeMin = 6 * 60
	alpha := 1 - math.Pow(0.5, 1.0/halfLifeMin)
	rate := float64(s.buckets[(s.cur-1+minutes)%minutes]) * 60 // last full minute, as per hour
	if s.baseHours == 0 {
		s.baseline = rate
	} else {
		s.baseline += alpha * (rate - s.baseline)
	}
	s.baseHours += 1.0 / 60
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
	return Info{Key: k, Level: s.Level(now), LastHour: s.LastHour(), Last5Min: s.LastMinutes(5),
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
