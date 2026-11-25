// Package llm calls the local LLM through its OpenAI-compatible API and runs small tool loops.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Message is a chat message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a call the model asked for.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool is something the model may call. Tools must be read-only.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON schema
	Run         func(ctx context.Context, args json.RawMessage) (string, error)
}

// Client is an OpenAI-compatible chat client.
type Client struct {
	URL, Token, Model string
	HTTP              *http.Client
}

// New returns a client.
func New(url, token, model string, timeout time.Duration) *Client {
	return &Client{URL: url, Token: token, Model: model, HTTP: &http.Client{Timeout: timeout}}
}

type chatReq struct {
	Model     string         `json:"model"`
	Messages  []Message      `json:"messages"`
	Tools     []any          `json:"tools,omitempty"`
	MaxTokens int            `json:"max_tokens"`
	Extra     map[string]any `json:"chat_template_kwargs,omitempty"`
	Format    map[string]any `json:"response_format,omitempty"`
}

type chatResp struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
}

// Chat sends one request. Thinking is turned off: with it on, answers took minutes.
func (c *Client) Chat(ctx context.Context, msgs []Message, tools []Tool, maxTokens int, jsonOut bool) (Message, error) {
	req := chatReq{Model: c.Model, Messages: msgs, MaxTokens: maxTokens, Extra: map[string]any{"enable_thinking": false}}
	for _, t := range tools {
		req.Tools = append(req.Tools, map[string]any{"type": "function", "function": map[string]any{
			"name": t.Name, "description": t.Description, "parameters": t.Parameters}})
	}
	if jsonOut {
		req.Format = map[string]any{"type": "json_object"}
	}
	b, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return Message{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	// the endpoint sits behind a proxy that rejects default library user agents
	hr.Header.Set("User-Agent", "noc-bot/0.1")
	if c.Token != "" {
		hr.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(hr)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return Message{}, fmt.Errorf("llm: HTTP %d: %.200s", resp.StatusCode, data)
	}
	var cr chatResp
	if err := json.Unmarshal(data, &cr); err != nil {
		return Message{}, err
	}
	if len(cr.Choices) == 0 {
		return Message{}, fmt.Errorf("llm: no choices")
	}
	return cr.Choices[0].Message, nil
}

// Step records one tool call and its result: the evidence behind an answer.
type Step struct {
	Tool   string `json:"tool"`
	Args   string `json:"args"`
	Result string `json:"result"`
}

// Run lets the model call tools for up to maxSteps rounds and returns its final message and the
// evidence it gathered.
func (c *Client) Run(ctx context.Context, msgs []Message, tools []Tool, maxSteps, maxTokens int, jsonOut bool) (string, []Step, error) {
	byName := map[string]Tool{}
	for _, t := range tools {
		byName[t.Name] = t
	}
	var steps []Step
	for i := 0; i <= maxSteps; i++ {
		offer := tools
		if i == maxSteps {
			// last round: no more tools; some models still try, so say it plainly
			offer = nil
			msgs = append(msgs, Message{Role: "user", Content: "No more tools. Answer now with what you have. / 道具はもう使えません。今ある情報だけで答えてください。"})
		}
		m, err := c.Chat(ctx, msgs, offer, maxTokens, jsonOut && len(offer) == 0)
		if err != nil {
			return "", steps, err
		}
		if len(m.ToolCalls) == 0 || offer == nil {
			if m.Content == "" {
				break
			}
			return m.Content, steps, nil
		}
		msgs = append(msgs, m)
		for _, tc := range m.ToolCalls {
			out := ""
			t, ok := byName[tc.Function.Name]
			if !ok {
				out = "error: no such tool"
			} else if r, err := t.Run(ctx, json.RawMessage(tc.Function.Arguments)); err != nil {
				out = "error: " + err.Error()
			} else {
				out = r
			}
			if len(out) > 6000 {
				out = out[:6000] + "…(truncated)"
			}
			steps = append(steps, Step{Tool: tc.Function.Name, Args: tc.Function.Arguments, Result: out})
			msgs = append(msgs, Message{Role: "tool", ToolCallID: tc.ID, Content: out})
		}
	}
	return "", steps, fmt.Errorf("llm: no answer after %d steps", maxSteps)
}
