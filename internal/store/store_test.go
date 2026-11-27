package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	id, err := s.Decision(ctx, now, "line", "line-v1", "tpl1", "text", map[string]any{"odd": true}, true)
	if err != nil || id == 0 {
		t.Fatal(err)
	}
	if err := s.ExpectOutcome(ctx, id, now.Add(-time.Second), "line", "tpl1", "text", true); err != nil {
		t.Fatal(err)
	}
	due, err := s.DueOutcomes(ctx, now)
	if err != nil || len(due) != 1 || due[0].Subject != "tpl1" {
		t.Fatalf("%v %+v", err, due)
	}
	s.OutcomeDone(ctx, id)
	if due, _ := s.DueOutcomes(ctx, now); len(due) != 0 {
		t.Fatal("should be done")
	}
	if err := s.Label(ctx, now, "line", "text", map[string]any{"odd": false}, "human", "U123", true, "known issue"); err != nil {
		t.Fatal(err)
	}
	s.Put(ctx, "drain", []byte("x"))
	if v, _ := s.Get(ctx, "drain"); string(v) != "x" {
		t.Fatal("kv")
	}
}

func TestRetractLabels(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(filepath.Join(t.TempDir(), "r.db"))
	defer s.Close()
	now := time.Now()
	s.Label(ctx, now, "line", "t", map[string]any{"odd": true}, "human", "U1", false, "incident 1126-0070")
	s.Label(ctx, now, "incident", "t", map[string]any{"severity": 2}, "human", "U1", true, "incident 1126-0070")
	s.Label(ctx, now, "line", "t", map[string]any{"odd": true}, "teacher", "noc-bot", false, "incident 1126-0070")
	if n, err := s.RetractLabels(ctx, "1126-0070", now); err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	// reopening an existing database keeps the column
	path := filepath.Join(t.TempDir(), "r2.db")
	a, _ := Open(path)
	a.Close()
	if b, err := Open(path); err != nil {
		t.Fatal(err)
	} else {
		b.Close()
	}
}

func TestLinesSearchAndPrune(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(filepath.Join(t.TempDir(), "l.db"))
	defer s.Close()
	now := time.Now()
	s.AddLines(ctx, []Line{
		{At: now.Add(-8 * 24 * time.Hour), Host: "eps-rt-01", Program: "Bgp", Msg: "old line"},
		{At: now.Add(-2 * time.Hour), Host: "eps-rt-01", Program: "Bgp", Msg: "%BGP-5-ADJCHANGE: peer 163.220.192.86 Idle"},
		{At: now.Add(-90 * time.Minute), Host: "noc-l2-06", Program: "Ebra", Msg: "Ethernet5 100%_down"},
		{At: now.Add(-time.Minute), Host: "eps-rt-01", Program: "Bgp", Msg: "%BGP-5-ADJCHANGE: peer 163.220.192.86 Established"},
	})
	ls, total, err := s.SearchLines(ctx, "eps-rt-01", "ADJCHANGE", now.Add(-3*time.Hour), now, 1)
	if err != nil || total != 2 || len(ls) != 1 || ls[0].Msg[len(ls[0].Msg)-11:] != "Established" {
		t.Fatalf("%v total=%d %+v", err, total, ls)
	}
	if _, total, _ := s.SearchLines(ctx, "", "100%_down", now.Add(-3*time.Hour), now, 10); total != 1 {
		t.Fatalf("literal %% and _ must match literally: %d", total)
	}
	s.PruneLines(ctx, 7*24*time.Hour)
	if _, total, _ := s.SearchLines(ctx, "", "", now.Add(-30*24*time.Hour), now, 10); total != 3 {
		t.Fatalf("prune: %d", total)
	}
}
