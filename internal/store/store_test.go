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
