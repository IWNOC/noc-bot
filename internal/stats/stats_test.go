package stats

import (
	"testing"
	"time"
)

// feed simulates `minutes` minutes with perMin events each, ticking every minute.
func feed(tr *Tracker, k Key, start time.Time, mins, perMin int) time.Time {
	now := start
	for i := 0; i < mins; i++ {
		for j := 0; j < perMin; j++ {
			tr.Observe(k, now.Add(time.Duration(j)*time.Second))
		}
		now = now.Add(time.Minute)
		tr.Tick(now)
	}
	return now
}

func TestNewThenNormalThenFarAbove(t *testing.T) {
	tr := NewTracker()
	k := Key{"template", "bgp"}
	start := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	tr.Observe(k, start)
	if i, _ := tr.Get(k, start.Add(time.Minute)); i.Level != LevelNew {
		t.Fatalf("got %s", i.Level)
	}
	// 3 hours of 1 every 10 minutes ≈ 6/h ... use 1/min for simplicity: 60/h
	now := feed(tr, k, start.Add(time.Minute), 180, 1)
	if i, _ := tr.Get(k, now); i.Level != LevelNormal {
		t.Fatalf("after steady rate got %s (base %.1f, hour %d)", i.Level, i.Baseline, i.LastHour)
	}
	now = feed(tr, k, now, 60, 12) // 720/h vs ~60/h
	if i, _ := tr.Get(k, now); i.Level != LevelFarAbove {
		t.Fatalf("after burst got %s (base %.1f, hour %d)", i.Level, i.Baseline, i.LastHour)
	}
}

func TestStopped(t *testing.T) {
	tr := NewTracker()
	k := Key{"sender", "kea"}
	start := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	now := feed(tr, k, start, 180, 5) // 300/h
	now = feed(tr, k, now, 61, 0)
	if i, _ := tr.Get(k, now); i.Level != LevelStopped {
		t.Fatalf("got %s (base %.1f)", i.Level, i.Baseline)
	}
}
