package teacher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/IWNOC/noc-bot/internal/llm"
)

func qs() map[string]Question {
	return map[string]Question{
		"odd":  {Type: "boolean", Instructions: "strange?"},
		"team": {Type: "choice", Instructions: "team?", Criteria: json.RawMessage(`{"l2l3":"net","server":"srv"}`)},
		"sev":  {Type: "score", Instructions: "impact", Criteria: json.RawMessage(`["none","some","many"]`)},
	}
}

func TestKnownAnswersWinWithoutLLM(t *testing.T) {
	k := NewKnown()
	k.Put("rec", "", map[string]any{"team": "l2l3", "odd": false, "bogus": 1}, "")
	tc := &Teacher{Known: k, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a, ev := tc.Answer(context.Background(), Request{Desk: "line", Text: "rec", Questions: qs()})
	if a["team"] != "l2l3" || a["odd"] != false || len(a) != 2 || ev["team"] == "" {
		t.Fatalf("got %v %v", a, ev)
	}
}

func TestLLMAnswersNeedEvidenceAndValidValues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("user agent required")
		}
		reply := `{"answers":{"odd":{"answer":true,"evidence":"FAR ABOVE NORMAL"},"team":{"answer":"wifi","evidence":"x"},"sev":{"answer":2,"evidence":""}}}`
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "```json\n" + reply + "\n```"}}}})
	}))
	defer srv.Close()
	tc := &Teacher{Known: NewKnown(), LLM: llm.New(srv.URL, "", "m", 5*time.Second), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a, _ := tc.Answer(context.Background(), Request{Desk: "line", Text: "rec", Questions: qs()})
	// odd: evidence and valid -> kept; team: "wifi" not an option -> dropped; sev: no evidence -> dropped
	if len(a) != 1 || a["odd"] != true {
		t.Fatalf("got %v", a)
	}
}

func TestAuth(t *testing.T) {
	tc := &Teacher{Token: "s", Known: NewKnown(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rr := httptest.NewRecorder()
	tc.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/teacher", strings.NewReader(`{}`)))
	if rr.Code != 401 {
		t.Fatalf("got %d", rr.Code)
	}
}

func TestShortAnswersAndTrailingProse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := "{\"answers\": {\"odd\": false, \"team\": \"l2l3\", \"sev\": {\"answer\": 1, \"evidence\": \"a {quoted} brace\"}}}\n\nWait — I must {double-check}"
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": reply}}}})
	}))
	defer srv.Close()
	tc := &Teacher{Known: NewKnown(), LLM: llm.New(srv.URL, "", "m", 5*time.Second), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a, ev := tc.Answer(context.Background(), Request{Desk: "line", Text: "rec", Questions: qs()})
	if a["odd"] != false || a["team"] != "l2l3" || a["sev"] != 1 || ev["odd"] == "" {
		t.Fatalf("got %v %v", a, ev)
	}
}

type memVerdicts map[string]map[string]any

func (m memVerdicts) Verdict(_ context.Context, desk, subject string) (map[string]any, bool) {
	v, ok := m[desk+"/"+subject]
	return v, ok
}
func (m memVerdicts) SetVerdict(_ context.Context, desk, subject string, a map[string]any) {
	m[desk+"/"+subject] = a
}

// Asked twice: only what both answers agree on is taught; the rest is reported as disputed.
// The agreed verdict is reused for the same subject without asking again.
func TestTwiceAgreeAndVerdictReuse(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		odd := calls%2 == 1 // flips between the two asks
		reply := fmt.Sprintf(`{"answers":{"odd":{"answer":%v,"evidence":"x"},"team":{"answer":"l2l3","evidence":"BGP"}}}`, odd)
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": reply}}}})
	}))
	defer srv.Close()
	var disputed []string
	k := NewKnown()
	k.Put("rec", "tpl1@h", nil, "")
	tc := &Teacher{Known: k, LLM: llm.New(srv.URL, "", "m", 5*time.Second), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Verdicts: memVerdicts{}, OnDispute: func(_, _, _ string, qs []string) { disputed = qs }}
	req := Request{Desk: "line", Text: "rec", Questions: map[string]Question{"odd": qs()["odd"], "team": qs()["team"]}}
	a, _ := tc.Answer(context.Background(), req)
	if len(a) != 1 || a["team"] != "l2l3" || len(disputed) != 1 || disputed[0] != "odd" {
		t.Fatalf("answers %v disputed %v", a, disputed)
	}
	before := calls
	req.Questions = map[string]Question{"team": qs()["team"]}
	a, ev := tc.Answer(context.Background(), req)
	if calls != before || a["team"] != "l2l3" || !strings.Contains(ev["team"], "before") {
		t.Fatalf("verdict not reused: calls %d->%d %v %v", before, calls, a, ev)
	}
}
