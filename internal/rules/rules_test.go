package rules

import (
	"testing"
	"time"

	"github.com/IWNOC/noc-bot/internal/config"
	"github.com/IWNOC/noc-bot/internal/stats"
)

func cfg() *config.Config {
	return &config.Config{
		Senders:  []config.Sender{{From: "10.110.0.1", Name: "eps-rt-01", Vendor: "arista"}},
		Expected: []config.Expected{{Host: "kea-primary*", Silence: 15 * time.Minute}},
		Known: []config.KnownIssue{
			{ID: "arista-clock", Reason: "accepted for the event", Vendor: "arista", ExpectSkew: 9 * time.Hour, SkewTolerance: time.Minute},
			{ID: "vyos-dhclient", Reason: "unused interface", Program: "dhclient", Template: `^DHCPDISCOVER on`},
		},
		Clock: config.Clock{MaxSkew: 2 * time.Minute},
	}
}

func TestKnownSkewSuppressedButDriftReported(t *testing.T) {
	c := cfg()
	e, err := New(c, stats.NewTracker())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	m := TemplateMeta{ID: "t1", Text: "x", Program: "Bgp", Host: "eps-rt-01", From: "10.110.0.1"}
	e.Observe(m, true, now, 9*time.Hour+10*time.Second)
	for _, s := range e.Evaluate(now) {
		if s.Kind == KindClockSkew {
			t.Fatalf("9h skew is a known issue: %+v", s)
		}
	}
	if e.Suppressed["arista-clock"] == 0 {
		t.Fatal("suppression not counted")
	}
	// the clock drifts much further: no longer the known issue
	e2, _ := New(c, stats.NewTracker())
	e2.Observe(m, true, now, 9*time.Hour+30*time.Minute)
	found := false
	for _, s := range e2.Evaluate(now) {
		found = found || s.Kind == KindClockSkew
	}
	if !found {
		t.Fatal("a skew outside the known band must be reported")
	}
}

func TestKnownTemplateNotNew(t *testing.T) {
	e, _ := New(cfg(), stats.NewTracker())
	now := time.Now()
	if s := e.Observe(TemplateMeta{ID: "d", Text: "DHCPDISCOVER on <*> to <IP> port <NUM> interval <NUM>", Program: "dhclient", Host: "eps-rt-02"}, true, now, 0); s != nil {
		t.Fatalf("known issue should not raise a new-template signal: %+v", s)
	}
	if s := e.Observe(TemplateMeta{ID: "b", Text: "%BGP-3-NOTIFICATION <*>", Program: "Bgp", Host: "eps-rt-01"}, true, now, 0); s == nil || s.Kind != KindNew {
		t.Fatalf("expected new signal, got %+v", s)
	}
}

func TestSilence(t *testing.T) {
	e, _ := New(cfg(), stats.NewTracker())
	t0 := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	e.Observe(TemplateMeta{ID: "k", Text: "x", Host: "kea-primary-abc"}, false, t0, 0)
	if got := e.Evaluate(t0.Add(10 * time.Minute)); len(got) != 0 {
		t.Fatalf("not silent yet: %+v", got)
	}
	got := e.Evaluate(t0.Add(20 * time.Minute))
	if len(got) != 1 || got[0].Kind != KindSilence {
		t.Fatalf("got %+v", got)
	}
}

func TestNoSilenceAlertRightAfterStart(t *testing.T) {
	e, _ := New(cfg(), stats.NewTracker())
	t0 := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	if got := e.Evaluate(t0); len(got) != 0 {
		t.Fatalf("must wait a silence period after start: %+v", got)
	}
	got := e.Evaluate(t0.Add(16 * time.Minute))
	if len(got) != 1 || got[0].Kind != KindSilence || got[0].Host != "kea-primary*" {
		t.Fatalf("got %+v", got)
	}
}

// A sender that used to talk and stopped is reported without being listed anywhere.
func TestSilenceFromBaselineWithoutConfig(t *testing.T) {
	tr := stats.NewTracker()
	e, _ := New(&config.Config{Clock: config.Clock{MaxSkew: 2 * time.Minute}}, tr)
	k := stats.Key{Kind: "sender", ID: "kea-primary-x"}
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 180; i++ {
		for j := 0; j < 5; j++ {
			tr.Observe(k, now.Add(time.Duration(j)*time.Second))
		}
		now = now.Add(time.Minute)
		tr.Tick(now)
	}
	for i := 0; i < 16; i++ {
		now = now.Add(time.Minute)
		tr.Tick(now)
	}
	got := e.Evaluate(now)
	if len(got) != 1 || got[0].Kind != KindSilence || got[0].Host != "kea-primary-x" {
		t.Fatalf("got %+v", got)
	}
}
