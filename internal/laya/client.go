// Package laya talks to laya-svc (the decision desks). Every call can fail: callers must keep
// working on rules and the LLM alone when laya is down.
package laya

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Answer is one question's answer as laya-svc returns it.
type Answer struct {
	Answer     any     `json:"answer"`
	Confidence float64 `json:"confidence"`
	Route      string  `json:"route"`
	Source     string  `json:"source"`    // "vega" or "teacher"
	Escalated  string  `json:"escalated"` // why the teacher was asked, if it was
	Abstain    bool    `json:"abstain"`
	Readout    string  `json:"readout"`        // "engine" or "head" (learned)
	Set        []any   `json:"prediction_set"` // option names, levels or true/false
}

// Bool reads a boolean answer.
func (a Answer) Bool() (bool, bool) {
	switch v := a.Answer.(type) {
	case bool:
		return v, true
	case string:
		return v == "true", v == "true" || v == "false"
	}
	return false, false
}

// String reads a choice answer.
func (a Answer) String() string {
	if s, ok := a.Answer.(string); ok {
		return s
	}
	return fmt.Sprint(a.Answer)
}

// Int reads a score answer.
func (a Answer) Int() (int, bool) {
	switch v := a.Answer.(type) {
	case float64:
		return int(v), true
	case string:
		var n int
		_, err := fmt.Sscan(v, &n)
		return n, err == nil
	}
	return 0, false
}

// Decision is a desk's answer to one text.
type Decision struct {
	Answers       map[string]Answer `json:"answers"`
	TeacherCalled bool              `json:"teacher_called"`
}

// Client calls laya-svc.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
	// Live lists desks where laya may answer alone once it has earned trust on a question (the
	// Escalator's measured line). Other desks stay in shadow: the teacher answers everything.
	Live map[string]bool
}

// New returns a client with the given timeout (decisions can wait on the teacher, so allow a minute+).
func New(url, token string, timeout time.Duration) *Client {
	return &Client{URL: url, Token: token, HTTP: &http.Client{Timeout: timeout}}
}

// unreached marks errors where laya-svc never produced an answer (connection, timeout, 5xx).
type unreached struct{ error }

func (u unreached) Unwrap() error { return u.error }

// Unreached reports whether err means laya-svc did not decide anything, so retrying is safe.
func Unreached(err error) bool {
	var u unreached
	return errors.As(err, &u)
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "noc-bot/0.1")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return unreached{err}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		err := fmt.Errorf("laya-svc %s: HTTP %d: %.200s", path, resp.StatusCode, data)
		if resp.StatusCode >= 500 {
			return unreached{err}
		}
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// Decide asks a desk.
func (c *Client) Decide(ctx context.Context, desk, text string) (Decision, error) {
	var d Decision
	body := map[string]any{"text": text}
	if c.Live[desk] {
		body["shadow"] = false
	}
	err := c.post(ctx, "/v1/desks/"+desk+"/decide", body, &d)
	return d, err
}

// Held lists the learning batches a desk is holding for a person: question -> why.
func (c *Client) Held(ctx context.Context, desk string) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/v1/desks/"+desk+"/held", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "noc-bot/0.1")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("laya-svc held: HTTP %d", resp.StatusCode)
	}
	raw := map[string]any{}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&raw); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range raw {
		out[k] = fmt.Sprint(v)
	}
	return out, nil
}

// ResolveHeld accepts (learn it) or discards a held batch.
func (c *Client) ResolveHeld(ctx context.Context, desk, question string, accept bool) error {
	verb := "discard"
	if accept {
		verb = "accept"
	}
	return c.post(ctx, "/v1/desks/"+desk+"/held/"+question+"/"+verb, map[string]any{}, nil)
}

// Origin of a label.
const (
	OriginHuman   = "human"
	OriginOutcome = "outcome"
)

// Feedback teaches a desk the right answers for a text it decided before.
func (c *Client) Feedback(ctx context.Context, desk, text string, answers map[string]any, correction bool, origin string) error {
	return c.post(ctx, "/v1/desks/"+desk+"/feedback", map[string]any{
		"text": text, "answers": answers, "correction": correction, "origin": origin}, nil)
}
