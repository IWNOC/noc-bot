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
