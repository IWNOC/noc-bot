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
	now := feed(tr, k, start, 180, 5) // 300/h, every minute
	now = feed(tr, k, now, 16, 0)
	if i, _ := tr.Get(k, now); i.Level != LevelStopped {
		t.Fatalf("got %s (base %.1f)", i.Level, i.Baseline)
	}
}

// A sender that logs in bursts is never STOPPED, however many lines a burst had.
func TestBurstySenderNeverStopped(t *testing.T) {
	tr := NewTracker()
	k := Key{"sender", "iw26-ntp-01"}
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 240; i++ {
		n := 0
		if i%30 == 0 {
			n = 40 // a burst every half hour
		}
		now = feed(tr, k, now, 1, n)
	}
	now = feed(tr, k, now, 60, 0)
	if i, _ := tr.Get(k, now); i.Level == LevelStopped {
		t.Fatalf("bursty sender reported STOPPED")
	}
}

func TestHourlyHistoryAndSnapshot(t *testing.T) {
	tr := NewTracker()
	k := Key{"template", "bgp"}
	start := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	now := feed(tr, k, start, 180, 1) // 60/h for 3 hours
	now = feed(tr, k, now, 120, 0)    // quiet for 2 hours
	snap, err := tr.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	tr2 := NewTracker()
	if err := tr2.Restore(snap); err != nil {
		t.Fatal(err)
	}
	_, cs, ok := tr2.Hourly(k, now.Add(-time.Second), 5)
	if !ok || cs[0] != 60 || cs[2] != 60 || cs[3] != 0 || cs[4] != 0 {
		t.Fatalf("hourly %v", cs)
	}
	if i, _ := tr2.Get(k, now); i.Baseline == 0 || i.Total != 180 {
		t.Fatalf("baseline not restored: %+v", i)
	}
}
