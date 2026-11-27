package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
	tools := []llm.Tool{
		{Name: "open_incidents", Description: "List incidents that are open right now.", Parameters: obj(map[string]any{}),
			Run: func(context.Context, json.RawMessage) (string, error) {
				var sb strings.Builder
				for _, in := range b.Incidents.Open() {
					fmt.Fprintf(&sb, "%s kind=%s host=%s since=%s hits=%d pattern=%q\n", in.ID, in.Kind, in.Host,
						in.Opened.In(jst).Format("01/02 15:04 JST"), in.Hits, in.Template)
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
		{Name: "pattern_history", Description: "Hour-by-hour counts of one log pattern (up to 72 hours, JST) and its latest lines. USE THIS for 'is it still happening', 'since when', 'when did it stop'. A pattern can be level NORMAL and still be an ongoing problem: NORMAL only means 'as often as recently'.",
			Parameters: obj(map[string]any{"template": str, "hours": map[string]any{"type": "integer", "description": "how far back, default 24"}}, "template"),
			Run: func(_ context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Template string
					Hours    int
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", err
				}
				id := b.resolvePattern(a.Template)
				if a.Hours <= 0 {
					a.Hours = 24
				}
				now := b.Now()
				ts, cs, ok := b.Stats.Hourly(stats.Key{Kind: "template", ID: id}, now, a.Hours)
				if !ok {
					return "", fmt.Errorf("no pattern %s (ids come from patterns_of_host)", id)
				}
				m, _ := b.Rules.Template(id)
				in, _ := b.Stats.Get(stats.Key{Kind: "template", ID: id}, now)
				var sb strings.Builder
				fmt.Fprintf(&sb, "pattern on %s: %s\nnow: %s. first seen %s, last seen %s, last 5 min: %d\ncounts per hour (JST, hour starting at):\n",
					m.Host, m.Text, now.In(jst).Format("01/02 15:04 JST"), in.FirstSeen.In(jst).Format("01/02 15:04"), in.LastSeen.In(jst).Format("01/02 15:04:05"), in.Last5Min)
				for i := range ts {
					fmt.Fprintf(&sb, "%s=%d ", ts[i].In(jst).Format("02日15時"), cs[i])
				}
				sb.WriteString("\n(history starts when noc-bot began counting; the current hour is partial)\nlatest lines:\n")
				b.mu.Lock()
				sb.WriteString(strings.Join(b.examples[id], "\n"))
				b.mu.Unlock()
				return sb.String(), nil
			}},
		{Name: "patterns_of_host", Description: "Log patterns a host sent: id, level against the recent normal, counts, when last seen. Times are JST.",
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
					fmt.Fprintf(&sb, "tpl:%s [%s] last_hour=%d normal_per_hour=%.0f last_seen=%s pattern=%q\n", t.Key.ID, t.Level, t.LastHour, t.Baseline,
						t.LastSeen.In(jst).Format("01/02 15:04 JST"), m.Text)
					if n++; n >= 30 {
						break
					}
				}
				if n == 0 {
					return "no patterns from that host", nil
				}
				return sb.String(), nil
			}},
		{Name: "recent_lines", Description: "The last few lines (received time, JST) of the patterns a host sent that are away from normal.",
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
		{Name: "site_notes", Description: "Facts about the network and past causes/fixes that operators confirmed in Slack.", Parameters: obj(map[string]any{}),
			Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
				notes, err := b.Store.Notes(ctx, 50, "fact", "resolution")
				if err != nil {
					return "", err
				}
				if len(notes) == 0 {
					return "no confirmed notes yet", nil
				}
				return strings.Join(notes, "\n"), nil
			}},
		{Name: "radius_summary", Description: "RADIUS authentication results per hour (accepted / rejected), JST: is Wi-Fi login working for people?",
			Parameters: obj(map[string]any{"hours": map[string]any{"type": "integer", "description": "default 12"}}),
			Run: func(_ context.Context, raw json.RawMessage) (string, error) {
				var a struct{ Hours int }
				json.Unmarshal(raw, &a)
				if a.Hours <= 0 {
					a.Hours = 12
				}
				now := b.Now()
				ts, acc, ok1 := b.Stats.Hourly(stats.Key{Kind: "metric", ID: "radius_accept"}, now, a.Hours)
				_, rej, ok2 := b.Stats.Hourly(stats.Key{Kind: "metric", ID: "radius_reject"}, now, a.Hours)
				if !ok1 && !ok2 {
					return "no RADIUS results seen since noc-bot started counting", nil
				}
				var sb strings.Builder
				sb.WriteString("hour (JST): accepted / rejected\n")
				for i := 0; i < a.Hours; i++ {
					var x, y uint32
					var t time.Time
					if ok1 {
						x, t = acc[i], ts[i]
					}
					if ok2 {
						y = rej[i]
					}
					if t.IsZero() {
						t = now.Add(-time.Duration(a.Hours-1-i) * time.Hour).Truncate(time.Hour)
					}
					fmt.Fprintf(&sb, "%s: %d / %d\n", t.In(jst).Format("02日15時"), x, y)
				}
				sb.WriteString("(the current hour is partial; rejects include wrong passwords and misconfigured devices retrying)\n")
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
	if b.Zabbix != nil {
		tools = append(tools,
			llm.Tool{Name: "zabbix_problems", Description: "Open problems in the NOC's Zabbix monitoring (ping, links, services, thresholds), newest first: what is actually broken or unreachable right now. Times JST.",
				Parameters: obj(map[string]any{"host": map[string]any{"type": "string", "description": "only this host (optional)"}}),
				Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
					var a struct{ Host string }
					json.Unmarshal(raw, &a)
					ps, err := b.Zabbix.Problems(ctx)
					if err != nil {
						return "", err
					}
					want := ""
					if a.Host != "" {
						want, _ = b.Zabbix.HostName(ctx, a.Host)
						if want == "" {
							want = a.Host
						}
					}
					var sb strings.Builder
					n := 0
					for _, p := range ps {
						if want != "" && p.Host != want {
							continue
						}
						ack := ""
						if p.Acked {
							ack = " (acknowledged)"
						}
						fmt.Fprintf(&sb, "%s [%s] %s: %s%s\n", p.Since.In(jst).Format("01/02 15:04 JST"), p.SeverityName(), p.Host, p.Name, ack)
						if n++; n >= 40 {
							break
						}
					}
					if n == 0 {
						return "no open problems in Zabbix" + map[bool]string{true: " for " + want}[want != ""], nil
					}
					return sb.String(), nil
				}},
			llm.Tool{Name: "zabbix_items", Description: "Latest values in Zabbix (interface status and traffic, CPU, DHCP pool usage, connected client counts...). match filters by item name (try Japanese and English words, e.g. 'Ethernet5', 'client', '端末', 'pool', 'ICMP'). Leave host empty to search every host: use that when you do not know which host holds a number.",
				Parameters: obj(map[string]any{"host": map[string]any{"type": "string", "description": "optional"}, "match": map[string]any{"type": "string"}}),
				Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
					var a struct{ Host, Match string }
					json.Unmarshal(raw, &a)
					is, err := b.Zabbix.Items(ctx, a.Host, a.Match)
					if err != nil {
						return "", err
					}
					var sb strings.Builder
					for _, i := range is {
						fmt.Fprintf(&sb, "%s = %s %s (%s)\n", i.Name, clipText(i.Value, 80), i.Units, i.At.In(jst).Format("15:04"))
					}
					if sb.Len() == 0 {
						return "no matching items", nil
					}
					return sb.String(), nil
				}})
	}
	return tools
}

// resolvePattern accepts a pattern id the way models write it: with or without "tpl:", quotes or
// the "@host" part. Without the host, the busiest pattern with that template id is used.
func (b *Bot) resolvePattern(raw string) string {
	id := strings.TrimPrefix(strings.Trim(strings.TrimSpace(raw), "\"'`[]"), "tpl:")
	if _, ok := b.Rules.Template(id); ok {
		return id
	}
	base, _, _ := strings.Cut(id, "@")
	for _, t := range b.Stats.All("template", b.Now()) { // busiest first
		if strings.HasPrefix(t.Key.ID, base+"@") {
			return t.Key.ID
		}
	}
	return id
}
