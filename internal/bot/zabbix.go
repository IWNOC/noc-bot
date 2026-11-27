package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/IWNOC/noc-bot/internal/laya"
	"github.com/IWNOC/noc-bot/internal/zabbix"
)

// zabbixOn lists Zabbix problems (warning and above... "average" = 3 and up) on a host that began
// around the given time: independent evidence that a log pattern matters.
func (b *Bot) zabbixOn(ctx context.Context, host string, around time.Time) []zabbix.Problem {
	if b.Zabbix == nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out []zabbix.Problem
	for _, p := range b.Zabbix.ProblemsOn(cctx, host, 3) {
		if d := around.Sub(p.Since); d > -15*time.Minute && d < 15*time.Minute {
			out = append(out, p)
		}
	}
	return out
}

func (b *Bot) zabbixWords(ctx context.Context, host string) string {
	if b.Zabbix == nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var parts []string
	for i, p := range b.Zabbix.ProblemsOn(cctx, host, 2) {
		if i >= 5 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s（%s、%s から）", p.Name, p.SeverityName(), p.Since.In(jst).Format("01/02 15:04")))
	}
	return strings.Join(parts, "；")
}

// zabbixOutcomes turns what monitoring saw into labels: when Zabbix opens a problem on a host, the
// error and state-change patterns that host logged in the half hour before it were worth noticing.
// This is evidence from outside the logs and the LLM, unlike the old "another alert followed" rule.
func (b *Bot) zabbixOutcomes(ctx context.Context, now time.Time) {
	if b.Zabbix == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ps, err := b.Zabbix.Problems(ctx)
	if err != nil {
		return
	}
	for _, p := range ps {
		if p.Severity < 3 || now.Sub(p.Since) > 10*time.Minute || now.Sub(p.Since) < 2*time.Minute {
			continue // wait two minutes so the lines around it have been judged; then handle once
		}
		key := fmt.Sprintf("%s|%s|%d", p.Host, p.Name, p.Since.Unix())
		b.mu.Lock()
		done := b.zbxDone[key]
		b.zbxDone[key] = true
		b.mu.Unlock()
		if done {
			continue
		}
		for _, sender := range b.knownHosts() {
			if name, ok := b.Zabbix.HostName(ctx, sender); !ok || name != p.Host {
				continue
			}
			ds, err := b.Store.LineDecisionsOf(ctx, sender, p.Since.Add(-30*time.Minute), p.Since.Add(2*time.Minute))
			if err != nil {
				continue
			}
			for _, d := range ds {
				var ans map[string]laya.Answer
				if json.Unmarshal([]byte(d.Answers), &ans) != nil {
					continue
				}
				if k := ans["kind"].String(); k != "error" && k != "state_change" {
					continue
				}
				label := map[string]any{"odd": true}
				note := "Zabbix: " + p.Host + " " + p.Name
				b.Store.Label(ctx, now, "line", d.Text, label, laya.OriginOutcome, "zabbix", false, note)
				if b.Laya != nil {
					if err := b.Laya.Feedback(ctx, "line", d.Text, label, false, laya.OriginOutcome); err != nil {
						b.Log.Warn("zabbix outcome feedback", "err", err)
					}
				}
			}
		}
	}
}
