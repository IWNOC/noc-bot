package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A model that keeps asking for tools must still end with an answer on the last round.
func TestRunForcesAnswer(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		msg := map[string]any{"role": "assistant", "content": "",
			"tool_calls": []any{map[string]any{"id": "c", "type": "function", "function": map[string]any{"name": "t", "arguments": "{}"}}}}
		if req["tools"] == nil {
			msg = map[string]any{"role": "assistant", "content": "final"}
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": msg}}})
	}))
	defer srv.Close()
	c := New(srv.URL, "", "m", 5*time.Second)
	tool := Tool{Name: "t", Parameters: map[string]any{"type": "object"}, Run: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }}
	out, steps, err := c.Run(context.Background(), []Message{{Role: "user", Content: "q"}}, []Tool{tool}, 2, 100, false)
	if err != nil || out != "final" || len(steps) != 2 || calls != 3 {
		t.Fatalf("out=%q steps=%d calls=%d err=%v", out, len(steps), calls, err)
	}
}
