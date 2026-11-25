package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/IWNOC/noc-bot/internal/llm"
	"github.com/IWNOC/noc-bot/internal/stats"
)

// Tools are the read-only tools the LLM may call. They read noc-bot's own memory; tools that query
// the syslog server's archive (trace a user, grep a day) belong to noc-query and come later.
func (b *Bot) Tools() []llm.Tool {
	obj := func(props map[string]any, req ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": req}
	}
	str := map[string]any{"type": "string"}
	return []llm.Tool{
		{Name: "open_incidents", Description: "List incidents that are open right now.", Parameters: obj(map[string]any{}),
			Run: func(context.Context, json.RawMessage) (string, error) {
				var sb strings.Builder
				for _, in := range b.Incidents.Open() {
					fmt.Fprintf(&sb, "%s kind=%s host=%s since=%s hits=%d pattern=%q\n", in.ID, in.Kind, in.Host,
						in.Opened.UTC().Format("15:04Z"), in.Hits, in.Template)
				}
				if sb.Len() == 0 {
					return "no open incidents", nil
				}
				return sb.String(), nil
			}},
		{Name: "sender_overview", Description: "Log volume per sender: last 5 minutes, last hour, and level against normal.", Parameters: obj(map[string]any{}),
			Run: func(context.Context, json.RawMessage) (string, error) {
				var sb strings.Builder
				for i, s := range b.Stats.All("sender", b.Now()) {
					if i >= 40 {
						break
					}
					fmt.Fprintf(&sb, "%s: 5min=%d hour=%d level=%s\n", s.Key.ID, s.Last5Min, s.LastHour, s.Level)
				}
				return sb.String(), nil
			}},
		{Name: "patterns_of_host", Description: "Log patterns a host sent, with their level against normal and an example line.",
			Parameters: obj(map[string]any{"host": str}, "host"),
			Run: func(_ context.Context, raw json.RawMessage) (string, error) {
				var a struct{ Host string }
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", err
				}
				var sb strings.Builder
				n := 0
				for _, t := range b.Stats.All("template", b.Now()) {
					m, ok := b.Rules.Template(t.Key.ID)
					if !ok || m.Host != a.Host {
						continue
					}
					fmt.Fprintf(&sb, "[%s] hour=%d pattern=%q example=%q\n", t.Level, t.LastHour, m.Text, m.Example)
					if n++; n >= 30 {
						break
					}
				}
				if n == 0 {
					return "no patterns from that host", nil
				}
				return sb.String(), nil
			}},
		{Name: "recent_lines", Description: "The last few raw lines of the patterns a host sent that are away from normal.",
			Parameters: obj(map[string]any{"host": str}, "host"),
			Run: func(_ context.Context, raw json.RawMessage) (string, error) {
				var a struct{ Host string }
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", err
				}
				var sb strings.Builder
				b.mu.Lock()
				defer b.mu.Unlock()
				for _, t := range b.Stats.All("template", b.Now()) {
					if t.Level == stats.LevelNormal {
						continue
					}
					if m, ok := b.Rules.Template(t.Key.ID); ok && m.Host == a.Host {
						for _, l := range b.examples[t.Key.ID] {
							sb.WriteString(l + "\n")
						}
					}
				}
				if sb.Len() == 0 {
					return "nothing unusual from that host", nil
				}
				return sb.String(), nil
			}},
		{Name: "known_issues", Description: "Accepted, known issues that should not alert by themselves.", Parameters: obj(map[string]any{}),
			Run: func(context.Context, json.RawMessage) (string, error) {
				var sb strings.Builder
				for _, k := range b.Rules.Known(b.Now()) {
					fmt.Fprintf(&sb, "%s: %s\n", k.ID, k.Reason)
				}
				return sb.String(), nil
			}},
	}
}
