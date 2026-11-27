package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/IWNOC/noc-bot/internal/incident"
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
					if ts[i].Add(time.Hour).Before(b.countSince) {
						continue // before counting began: unknown, not zero
					}
					fmt.Fprintf(&sb, "%s=%d ", ts[i].In(jst).Format("02日15時"), cs[i])
				}
				fmt.Fprintf(&sb, "\nIMPORTANT: counting began at %s. Nothing is known about earlier hours: do NOT say the pattern started then. For earlier times use search_logs, past_alerts or notebook_search, or say it is unknown. The current hour is partial.\nlatest lines:\n", b.countSince.In(jst).Format("01/02 15:04 JST"))
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
		{Name: "search_logs", Description: "Search the log lines noc-bot received in the last 7 days (device and service events; not DNS queries or NAT/DHCP packet logs). Use for 'what happened around 15:00 yesterday', 'did this MAC/interface/peer appear before'. Times are JST.",
			Parameters: obj(map[string]any{
				"host":     map[string]any{"type": "string", "description": "sender host name, exact (optional)"},
				"contains": map[string]any{"type": "string", "description": "text the line contains, e.g. 'Ethernet5', 'BGP', a MAC (optional)"},
				"from":     map[string]any{"type": "string", "description": "start, JST, 'YYYY-MM-DD HH:MM' or 'HH:MM' for today (default: 1 hour ago)"},
				"to":       map[string]any{"type": "string", "description": "end, same format (default: now)"},
				"limit":    map[string]any{"type": "integer", "description": "max lines, default 40, at most 120"}}),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Host, Contains, From, To string
					Limit                    int
				}
				json.Unmarshal(raw, &a)
				now := b.Now()
				from, to := now.Add(-time.Hour), now
				if a.From != "" {
					t, err := parseJST(a.From, now)
					if err != nil {
						return "", err
					}
					from = t
				}
				if a.To != "" {
					t, err := parseJST(a.To, now)
					if err != nil {
						return "", err
					}
					to = t
				}
				if a.Limit <= 0 {
					a.Limit = 40
				}
				if a.Limit > 120 {
					a.Limit = 120
				}
				ls, total, err := b.Store.SearchLines(ctx, a.Host, a.Contains, from, to, a.Limit)
				if err != nil {
					return "", err
				}
				var sb strings.Builder
				fmt.Fprintf(&sb, "%d lines matched between %s and %s JST", total, from.In(jst).Format("01/02 15:04"), to.In(jst).Format("01/02 15:04"))
				if total > len(ls) {
					fmt.Fprintf(&sb, " (showing the last %d; narrow the time or text to see others)", len(ls))
				}
				sb.WriteString("\n")
				for _, l := range ls {
					fmt.Fprintf(&sb, "%s %s %s: %s\n", l.At.In(jst).Format("01/02 15:04:05"), l.Host, l.Program, clipText(l.Msg, 300))
				}
				return sb.String(), nil
			}},
		{Name: "past_alerts", Description: "Alerts noc-bot raised in a time range (also ones from before a restart): time, host, how it was delivered, headline. Times JST.",
			Parameters: obj(map[string]any{
				"from": map[string]any{"type": "string", "description": "start, JST 'YYYY-MM-DD HH:MM' or 'HH:MM' (default: 24 hours ago)"},
				"to":   map[string]any{"type": "string", "description": "end (default: now)"},
				"host": map[string]any{"type": "string", "description": "only this host (optional)"}}),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct{ From, To, Host string }
				json.Unmarshal(raw, &a)
				now := b.Now()
				from, to := now.Add(-24*time.Hour), now
				if a.From != "" {
					if t, err := parseJST(a.From, now); err == nil {
						from = t
					}
				}
				if a.To != "" {
					if t, err := parseJST(a.To, now); err == nil {
						to = t
					}
				}
				bodies, err := b.Store.IncidentsBetween(ctx, from, to, 300)
				if err != nil {
					return "", err
				}
				var sb strings.Builder
				n := 0
				for _, raw := range bodies {
					var in incident.Incident
					if json.Unmarshal([]byte(raw), &in) != nil || (a.Host != "" && in.Host != a.Host) {
						continue
					}
					tier := map[string]string{TierNow: "told at once", TierDigest: "in a digest", TierSilent: "not posted"}[in.Tier]
					fmt.Fprintf(&sb, "%s %s %s [%s] %s\n", in.Opened.In(jst).Format("01/02 15:04"), in.ID, in.Host, tier, b.friendlyName(ctx, in.Title, in.TplID, in.Template, 110))
					if n++; n >= 60 {
						sb.WriteString("…more; narrow the range\n")
						break
					}
				}
				if n == 0 {
					return "no alerts in that range", nil
				}
				return sb.String(), nil
			}},
		{Name: "notebook_search", Description: "Search everything noc-bot has written down or been taught (investigator's notes, meanings, who outside parties are, operators' facts, handover notes) for a word: a host, an interface, an AS, a topic.",
			Parameters: obj(map[string]any{"word": str}, "word"),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct{ Word string }
				json.Unmarshal(raw, &a)
				if strings.TrimSpace(a.Word) == "" {
					return "", fmt.Errorf("give a word")
				}
				ns, err := b.Store.SearchNotes(ctx, strings.TrimSpace(a.Word), 25)
				if err != nil {
					return "", err
				}
				if len(ns) == 0 {
					return "nothing written about " + a.Word, nil
				}
				for i := range ns {
					ns[i] = clipText(ns[i], 500)
				}
				return strings.Join(ns, "\n"), nil
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
					if t.Add(time.Hour).Before(b.countSince) {
						continue
					}
					fmt.Fprintf(&sb, "%s: %d / %d\n", t.In(jst).Format("02日15時"), x, y)
				}
				fmt.Fprintf(&sb, "(counting began at %s, earlier hours are unknown; the current hour is partial; rejects include wrong passwords and misconfigured devices retrying)\n", b.countSince.In(jst).Format("01/02 15:04 JST"))
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

// parseJST reads "YYYY-MM-DD HH:MM", "MM/DD HH:MM" or "HH:MM" (today) as JST.
func parseJST(v string, now time.Time) (time.Time, error) {
	v = strings.TrimSpace(strings.ReplaceAll(v, "T", " "))
	n := now.In(jst)
	for _, f := range []string{"2006-01-02 15:04", "2006-01-02 15:04:05", "2006/01/02 15:04"} {
		if t, err := time.ParseInLocation(f, v, jst); err == nil {
			return t, nil
		}
	}
	if t, err := time.ParseInLocation("01/02 15:04", v, jst); err == nil {
		return time.Date(n.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, jst), nil
	}
	if t, err := time.ParseInLocation("15:04", v, jst); err == nil {
		return time.Date(n.Year(), n.Month(), n.Day(), t.Hour(), t.Minute(), 0, 0, jst), nil
	}
	return time.Time{}, fmt.Errorf("time %q: use 'YYYY-MM-DD HH:MM' or 'HH:MM' (JST)", v)
}
