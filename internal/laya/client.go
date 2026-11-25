// Package laya talks to laya-svc (the decision desks). Every call can fail: callers must keep
// working on rules and the LLM alone when laya is down.
package laya

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Answer is one question's answer as laya-svc returns it.
type Answer struct {
	Answer     any      `json:"answer"`
	Confidence float64  `json:"confidence"`
	Route      string   `json:"route"`
	Source     string   `json:"source"`    // "vega" or "teacher"
	Escalated  string   `json:"escalated"` // why the teacher was asked, if it was
	Abstain    bool     `json:"abstain"`
	Readout    string   `json:"readout"` // "engine" or "head" (learned)
	Set        []string `json:"prediction_set"`
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
}

// New returns a client with the given timeout (decisions can wait on the teacher, so allow a minute+).
func New(url, token string, timeout time.Duration) *Client {
	return &Client{URL: url, Token: token, HTTP: &http.Client{Timeout: timeout}}
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
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("laya-svc %s: HTTP %d: %.200s", path, resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// Decide asks a desk.
func (c *Client) Decide(ctx context.Context, desk, text string) (Decision, error) {
	var d Decision
	err := c.post(ctx, "/v1/desks/"+desk+"/decide", map[string]any{"text": text}, &d)
	return d, err
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
