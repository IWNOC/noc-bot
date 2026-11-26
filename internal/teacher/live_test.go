//go:build live

package teacher

// Re-asks the teacher about stored line-desk texts with the current prompt.
//   REPLAY_DB=... LLM_URL=... LLM_TOKEN=... go test -tags live -run TestReplayTeacher -v ./internal/teacher

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/IWNOC/noc-bot/internal/llm"
)

func TestReplayTeacher(t *testing.T) {
	db, err := sql.Open("sqlite", os.Getenv("REPLAY_DB"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT d.subject, d.text, d.answers FROM decisions d WHERE d.desk='line' AND d.id IN (SELECT max(id) FROM decisions WHERE desk='line' GROUP BY subject)`)
	if err != nil {
		t.Fatal(err)
	}
	type rec struct{ subj, text, old string }
	var recs []rec
	for rows.Next() {
		var r rec
		rows.Scan(&r.subj, &r.text, &r.old)
		recs = append(recs, r)
	}
	var qs map[string]Question
	json.Unmarshal([]byte(`{
	 "odd": {"type": "boolean", "instructions": "Would an experienced network operator watching these logs think 'that is strange, someone should look at this'?"},
	 "kind": {"type": "choice", "instructions": "What kind of log line is this?", "criteria": {"error": "something failed or was refused", "state_change": "a session, link, peer or service went up or down", "info": "normal activity worth keeping", "noise": "routine chatter nobody needs to read"}}}`), &qs)
	tc := &Teacher{Known: NewKnown(), LLM: llm.New(os.Getenv("LLM_URL"), os.Getenv("LLM_TOKEN"), "Qwen3.8-Flash-Next-oQ4e-fp16-mtp", 90*time.Second),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	type res struct {
		rec
		odd  any
		kind any
	}
	out := make([]res, len(recs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	for i, r := range recs {
		wg.Add(1)
		go func(i int, r rec) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			a, _ := tc.Answer(context.Background(), Request{Desk: "line", Text: r.text, Questions: qs})
			out[i] = res{r, a["odd"], a["kind"]}
		}(i, r)
	}
	wg.Wait()
	oldOdd, newOdd := 0, 0
	for _, r := range out {
		was := strings.Contains(r.old, `"odd":{"answer":true`)
		if was {
			oldOdd++
		}
		if r.odd == true {
			newOdd++
		}
		first := strings.SplitN(r.text, "\n", 3)
		line := ""
		if len(first) > 1 {
			line = strings.TrimSpace(first[1])
		}
		if was || r.odd == true {
			t.Logf("old odd=%-5v new odd=%-5v kind=%-12v %s", was, r.odd, r.kind, clip(line, 90))
		}
	}
	t.Logf("templates %d: odd before %d (%.0f%%), now %d (%.0f%%)", len(out), oldOdd, 100*float64(oldOdd)/float64(len(out)), newOdd, 100*float64(newOdd)/float64(len(out)))
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
