package bot

// Knowledge the investigator wrote in its notebook, read wherever a pattern is judged or shown:
// the text laya and its teacher read, alerts, and conversation. Nothing here looks anything up:
// what to investigate is the investigator's call (investigator.go).

import (
	"context"
	"net/netip"
	"regexp"
	"strings"
)

var (
	asnRe  = regexp.MustCompile(`(?i)\bAS\s?(\d{1,10})\b`)
	addrRe = regexp.MustCompile(`(?i)\b\d{1,3}(?:\.\d{1,3}){3}\b|(?:[0-9a-f]{1,4}:){2,7}[0-9a-f]{1,4}|(?:[0-9a-f]{1,4}:)+:[0-9a-f:]*`)
)

// knowledge returns the latest note of a kind about a subject.
func (b *Bot) knowledge(ctx context.Context, kind, subject string) (string, bool) {
	rows, err := b.Store.NotesAbout(ctx, kind, subject, 1)
	if err != nil || len(rows) == 0 {
		return "", false
	}
	return rows[0], true
}

// background is what the notebook says about a pattern (its meaning) and about the outside
// parties (AS numbers, public addresses) that appear in an example line.
func (b *Bot) background(ctx context.Context, tplID, example string) (meaning string, ents []string) {
	if tplID != "" {
		var ok bool
		if meaning, ok = b.knowledge(ctx, "meaning", "tpl:"+tplID); !ok {
			// notes written before patterns were per host, or for the same message on another host
			base, _, _ := strings.Cut(tplID, "@")
			meaning, _ = b.knowledge(ctx, "meaning", "tpl:"+base)
		}
	}
	seen := map[string]bool{}
	add := func(k string) {
		if seen[k] {
			return
		}
		seen[k] = true
		if got, ok := b.knowledge(ctx, "entity", k); ok {
			ents = append(ents, got)
		}
	}
	for _, m := range asnRe.FindAllStringSubmatch(example, -1) {
		add("AS" + m[1])
	}
	for _, m := range addrRe.FindAllString(example, -1) {
		if a, err := netip.ParseAddr(strings.TrimRight(m, ":.")); err == nil {
			add(a.String())
		}
	}
	return meaning, ents
}
