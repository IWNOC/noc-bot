package incident

import (
	"testing"
	"time"

	"github.com/IWNOC/noc-bot/internal/rules"
)

func TestLifecycle(t *testing.T) {
	m := NewManager()
	t0 := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	sig := rules.Signal{Kind: rules.KindRate, Key: "rate:bgp", Host: "eps-rt-01", At: t0}
	in, opened := m.Signal(sig)
	if !opened || in.State != Open {
		t.Fatal("should open")
	}
	m.Update("rate:bgp", func(i *Incident) { i.Notified = t0 })
	sig.At = t0.Add(5 * time.Minute)
	if _, opened := m.Signal(sig); opened {
		t.Fatal("same key must not reopen")
	}
	// one reminder after an hour of activity
	for i := 1; i <= 60; i++ {
		sig.At = t0.Add(time.Duration(5+i) * time.Minute)
		m.Signal(sig)
	}
	ev := m.Tick(t0.Add(66 * time.Minute))
	if len(ev) != 1 || ev[0].Kind != EvReminder {
		t.Fatalf("got %+v", ev)
	}
	// quiet for 15 minutes -> resolved
	ev = m.Tick(t0.Add(65*time.Minute + 15*time.Minute))
	if len(ev) != 1 || ev[0].Kind != EvResolved {
		t.Fatalf("got %+v", ev)
	}
	// comes back -> a new incident
	sig.At = t0.Add(3 * time.Hour)
	in2, opened := m.Signal(sig)
	if !opened || in2.ID == in.ID {
		t.Fatal("should be a new incident")
	}
}
