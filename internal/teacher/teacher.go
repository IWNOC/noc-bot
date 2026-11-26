// Package teacher answers laya-svc's Escalator when laya is unsure. In order:
//  1. facts noc-bot already knows for that text (the team map, known issues): exact answers;
//  2. the local LLM, with read-only tools, which must give evidence for each answer;
//  3. nothing: a question without evidence is left out, so laya learns nothing from a guess.
package teacher

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/IWNOC/noc-bot/internal/llm"
)

// Known holds exact answers noc-bot can give for a text it is about to send to laya.
type Known struct {
	mu  sync.Mutex
	m   map[[32]byte]knownEntry
	ttl time.Duration
	now func() time.Time
}

type knownEntry struct {
	answers map[string]any
	context string // extra facts in words for the LLM
	at      time.Time
}

// NewKnown returns an empty registry.
func NewKnown() *Known {
	return &Known{m: map[[32]byte]knownEntry{}, ttl: 30 * time.Minute, now: time.Now}
}

// Put registers exact answers and context for a text before it is sent to laya.
func (k *Known) Put(text string, answers map[string]any, context string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	for h, e := range k.m {
		if now.Sub(e.at) > k.ttl {
			delete(k.m, h)
		}
	}
	k.m[sha256.Sum256([]byte(text))] = knownEntry{answers, context, now}
}

func (k *Known) get(text string) (knownEntry, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	e, ok := k.m[sha256.Sum256([]byte(text))]
	return e, ok
}

// Question as laya-svc sends it.
type Question struct {
	Type         string          `json:"type"`
	Instructions string          `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria"`
}

// options lists valid answers for choice and score questions.
func (q Question) options() []string {
	if len(q.Criteria) == 0 {
		return nil
	}
	var m map[string]string
	if json.Unmarshal(q.Criteria, &m) == nil {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		return out
	}
	var l []string
	if json.Unmarshal(q.Criteria, &l) == nil {
		out := make([]string, len(l))
		for i := range l {
			out[i] = fmt.Sprint(i)
		}
		return out
	}
	return nil
}

// valid normalises an answer or reports it unusable.
func (q Question) valid(v any) (any, bool) {
	switch q.Type {
	case "boolean":
		switch x := v.(type) {
		case bool:
			return x, true
		case string:
			if x == "true" || x == "false" {
				return x == "true", true
			}
		}
	case "choice":
		s, ok := v.(string)
		if !ok {
			return nil, false
		}
		for _, o := range q.options() {
			if o == s {
				return s, true
			}
		}
	case "score":
		var n int
		switch x := v.(type) {
		case float64:
			n = int(x)
		case string:
			if _, err := fmt.Sscan(x, &n); err != nil {
				return nil, false
			}
		default:
			return nil, false
		}
		if n >= 0 && n < len(q.options()) {
			return n, true
		}
	}
	return nil, false
}

// Request is laya-svc's call.
type Request struct {
	Desk      string              `json:"desk"`
	Text      string              `json:"text"`
	Questions map[string]Question `json:"questions"`
}

// Teacher is the HTTP handler.
type Teacher struct {
	Token string
	Known *Known
	LLM   *llm.Client // nil: rules only
	Tools []llm.Tool
	Log   *slog.Logger
	// OnAnswer, when set, is told every answer the teacher gave and its evidence (for the store).
	OnAnswer func(desk, text string, answers map[string]any, evidence map[string]string)
}

func (t *Teacher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if t.Token != "" {
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte("Bearer "+t.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	var req Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	answers, evidence := t.Answer(r.Context(), req)
	if t.OnAnswer != nil && len(answers) > 0 {
		t.OnAnswer(req.Desk, req.Text, answers, evidence)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"answers": answers, "evidence": evidence})
}

// Answer answers what it can.
func (t *Teacher) Answer(ctx context.Context, req Request) (map[string]any, map[string]string) {
	answers := map[string]any{}
	evidence := map[string]string{}
	extra := ""
	if e, ok := t.Known.get(req.Text); ok {
		extra = e.context
		for q, v := range e.answers {
			if qq, ok := req.Questions[q]; ok {
				if vv, ok := qq.valid(v); ok {
					answers[q] = vv
					evidence[q] = "noc-bot configuration (team map / confirmed known issue)"
				}
			}
		}
	}
	rest := map[string]Question{}
	for q, qq := range req.Questions {
		if _, done := answers[q]; !done {
			rest[q] = qq
		}
	}
	if len(rest) == 0 || t.LLM == nil {
		return answers, evidence
	}
	got, ev, err := t.ask(ctx, req.Desk, req.Text, extra, rest)
	if err != nil {
		t.Log.Warn("teacher llm", "desk", req.Desk, "err", err)
		return answers, evidence
	}
	for q, v := range got {
		answers[q] = v
		evidence[q] = ev[q]
	}
	// A line the teacher itself calls routine (noise/info) is not odd, whatever it said about odd:
	// keep its labels consistent so laya does not learn to over-alert from the teacher.
	if k, ok := answers["kind"].(string); ok && (k == "noise" || k == "info") {
		if _, asked := req.Questions["odd"]; asked {
			answers["odd"] = false
			evidence["odd"] = "kind=" + k + " (routine, so not odd)"
		}
	}
	return answers, evidence
}

const system = `You label records for a network operations centre (NOC) running a three-day event network.
Base rate: in a healthy network well over 90% of new log patterns need no action at all (routine jobs,
sessions opening and closing, reloads, keepalives). Say "odd" only when an experienced operator would
actually go and look: errors, failures, refusals, things going down, or a sustained change that matters.
Answer each question about the record. Use the tools to check facts when they help.
For every answer you give, quote the evidence: the few words in the record or the tool result that decide it
(at most 150 characters; quote, do not explain).
If you cannot back an answer with evidence, leave that question out. Never guess.
Reply with JSON only: {"answers": {"<question id>": {"answer": <value>, "evidence": "<quote>"}}}
Values: boolean questions take true or false; choice questions take one option name exactly as listed;
score questions take the level number.`

func (t *Teacher) ask(ctx context.Context, desk, text, extra string, qs map[string]Question) (map[string]any, map[string]string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Desk: %s\nRecord:\n%s\n", desk, text)
	if extra != "" {
		fmt.Fprintf(&b, "Additional facts from noc-bot:\n%s\n", extra)
	}
	b.WriteString("Questions:\n")
	for id, q := range qs {
		fmt.Fprintf(&b, "- %s (%s): %s", id, q.Type, q.Instructions)
		if len(q.Criteria) > 0 {
			fmt.Fprintf(&b, " Options: %s", q.Criteria)
		}
		b.WriteString("\n")
	}
	content, _, err := t.LLM.Run(ctx, []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: b.String()}}, t.Tools, 3, 2500, true) // long evidence quotes cut replies off mid-JSON; quotes are now capped too
	if err != nil {
		return nil, nil, err
	}
	// The model does not always answer in the asked shape: {"q": {"answer": .., "evidence": ..}}
	// or the short {"q": value}, sometimes followed by its own musings. Read the first JSON object
	// and accept both shapes; a bare value carries no quote, so it is marked as such.
	var out struct {
		Answers map[string]json.RawMessage `json:"answers"`
	}
	if err := json.NewDecoder(strings.NewReader(firstObject(content))).Decode(&out); err != nil || out.Answers == nil {
		return nil, nil, fmt.Errorf("teacher: unparsable reply: %.200s", content)
	}
	answers, evidence := map[string]any{}, map[string]string{}
	for id, raw := range out.Answers {
		q, ok := qs[id]
		if !ok {
			continue
		}
		var full struct {
			Answer   any    `json:"answer"`
			Evidence string `json:"evidence"`
		}
		var val any
		ev := ""
		if json.Unmarshal(raw, &full) == nil && full.Answer != nil {
			val, ev = full.Answer, strings.TrimSpace(full.Evidence)
			if ev == "" {
				continue // asked for a quote and gave none: a guess
			}
		} else if json.Unmarshal(raw, &val) != nil {
			continue
		} else {
			ev = "(short answer, no quote)"
		}
		if v, ok := q.valid(val); ok {
			answers[id], evidence[id] = v, ev
		}
	}
	return answers, evidence, nil
}

// firstObject returns the first complete JSON object in s (ignoring code fences and trailing prose).
func firstObject(s string) string {
	i := strings.IndexByte(s, '{')
	if i < 0 {
		return s
	}
	depth, inStr, esc := 0, false, false
	for j := i; j < len(s); j++ {
		c := s[j]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[i : j+1]
			}
		}
	}
	return s[i:]
}
