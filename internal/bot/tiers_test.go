package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/IWNOC/noc-bot/internal/incident"
	"github.com/IWNOC/noc-bot/internal/laya"
	"github.com/IWNOC/noc-bot/internal/rules"
	"github.com/IWNOC/noc-bot/internal/slackapp"
	"github.com/IWNOC/noc-bot/internal/statebuilder"
)

func lineDecision(t *testing.T, b *Bot, tpl, kind string, odd bool) {
	t.Helper()
	b.Store.Decision(context.Background(), time.Now(), "line", statebuilder.Version, tpl, "x", map[string]laya.Answer{
		"kind": {Answer: kind, Source: "teacher"}, "odd": {Answer: odd, Source: "teacher"}}, true)
}

func TestTierByMeaning(t *testing.T) {
	b, _, _, _ := setup(t, "{}")
	ctx := context.Background()
	mk := func(kind, tpl string) (incident.Incident, rules.Signal) {
		s := rules.Signal{Kind: kind, Key: kind + ":" + tpl, Host: "eps-rt-01", TplID: tpl, Template: tpl, At: time.Now()}
		in, _ := b.Incidents.Signal(s)
		return in, s
	}
	lineDecision(t, b, "logrotate", "noise", false)
	lineDecision(t, b, "badas", "error", true)
	lineDecision(t, b, "sessions", "info", false)
	cases := []struct {
		kind, tpl, want string
	}{
		{rules.KindRate, "logrotate", TierSilent}, // routine
		{"odd", "badas", TierNow},                 // an error that looks odd
		{rules.KindRate, "sessions", TierDigest},  // info
		{rules.KindSilence, "", TierNow},          // a steady sender went quiet
		{rules.KindClockSkew, "", TierDigest},
	}
	for _, c := range cases {
		in, s := mk(c.kind, c.tpl)
		if got, _, _ := b.decideTier(ctx, in, s); got != c.want {
			t.Errorf("%s/%s: got %s want %s", c.kind, c.tpl, got, c.want)
		}
	}
	// an error right after a config change on the same host is "now" even if not judged odd
	lineDecision(t, b, "peerdown", "state_change", false)
	b.noteConfigChange("eps-rt-01", "%SYS-5-CONFIG_I: Configured from console by iw26 on vty4", time.Now().Add(-2*time.Minute))
	in, s := mk(rules.KindRate, "peerdown")
	if got, why, _ := b.decideTier(ctx, in, s); got != TierNow || !strings.Contains(why, "設定変更") {
		t.Fatalf("config change: %s %q", got, why)
	}
}

func TestPersonSetsTierAndCanUndo(t *testing.T) {
	b, _, _, in := setup(t, "{}")
	ctx := context.Background()
	reply, key := b.OnAnswer(ctx, in.ID, slackapp.ActDigest, "", "U1")
	if key == "" || !strings.Contains(reply, "まとめ") {
		t.Fatalf("%q %q", reply, key)
	}
	s := rules.Signal{Kind: "odd", TplID: in.TplID}
	if got, why, _ := b.decideTier(ctx, in, s); got != TierDigest || why == "" {
		t.Fatalf("person's choice not applied: %s", got)
	}
	b.OnUndo(ctx, key, "U1")
	if r := b.tierRules(ctx)[key]; r.Tier == TierDigest {
		t.Fatalf("undo did not revert: %+v", r)
	}
}

// Ignored "now" alerts demote info/noise kinds only; errors are never demoted without a person.
func TestIgnoredDemotesOnlyRoutineKinds(t *testing.T) {
	b, _, _, _ := setup(t, "{}")
	ctx := context.Background()
	lineDecision(t, b, "chatter", "info", false)
	lineDecision(t, b, "bgpdown", "error", true)
	now := time.Now()
	for i := 0; i < 3; i++ {
		for _, tpl := range []string{"chatter", "bgpdown"} {
			s := rules.Signal{Kind: "odd", Key: "k" + tpl + string(rune('a'+i)), Host: "h", TplID: tpl, At: now}
			in, _ := b.Incidents.Signal(s)
			b.Incidents.Update(in.Key, func(x *incident.Incident) { x.Tier, x.Thread, x.Notified = TierNow, "ts", now.Add(-2*time.Hour) })
		}
		b.judgeAttention(ctx, now)
	}
	rs := b.tierRules(ctx)
	if rs["tpl:chatter"].Tier != TierDigest || !rs["tpl:chatter"].Auto {
		t.Fatalf("info kind not demoted: %+v", rs["tpl:chatter"])
	}
	if rs["tpl:bgpdown"].Tier != "" {
		t.Fatalf("an error kind was demoted without a person: %+v", rs["tpl:bgpdown"])
	}
}

func TestDigestListsItemsAndDemotions(t *testing.T) {
	b, fs, _, _ := setup(t, `{"gist":"落ち着いています"}`)
	ctx := context.Background()
	now := time.Now()
	for _, h := range []string{"eps-rt-02", "noc-l2-06"} {
		in, _ := b.Incidents.Signal(rules.Signal{Kind: rules.KindRate, Key: "rate:" + h, Host: h, Template: "pattern on " + h, At: now})
		b.Incidents.Update(in.Key, func(x *incident.Incident) { x.Tier = TierDigest })
	}
	b.digest(ctx, now)
	if len(fs.posts) != 1 || !strings.Contains(fs.posts[0], "この30分のまとめ") || !strings.Contains(fs.posts[0], "noc-l2-06") || !strings.Contains(fs.posts[0], "落ち着いています") {
		t.Fatalf("posts %v", fs.posts)
	}
	b.digest(ctx, now.Add(time.Minute)) // nothing new: no second digest
	if len(fs.posts) != 1 {
		t.Fatalf("empty digest posted: %v", fs.posts)
	}
}

func TestStormIsBundled(t *testing.T) {
	b, fs, _, _ := setup(t, "{}")
	now := time.Now()
	for i := 0; i < stormMax; i++ {
		if b.stormy(now) {
			t.Fatalf("stormy too early at %d", i)
		}
	}
	if !b.stormy(now) {
		t.Fatal("cap not reached")
	}
	in, _ := b.Incidents.Signal(rules.Signal{Kind: "odd", Key: "x", Host: "h", Template: "t", At: now})
	b.stormQ = append(b.stormQ, in, in)
	b.flushStorm(context.Background())
	if len(fs.posts) != 1 || !strings.Contains(fs.posts[0], "2 件") {
		t.Fatalf("posts %v", fs.posts)
	}
}

func TestSameHostStoryJoinsThread(t *testing.T) {
	b, _, _, _ := setup(t, "{}")
	now := time.Now()
	first, _ := b.Incidents.Signal(rules.Signal{Kind: "odd", Key: "a", Host: "eps-rt-02", TplID: "dhcp1", At: now})
	b.Incidents.Update(first.Key, func(x *incident.Incident) { x.Tier, x.Thread, x.Notified = TierNow, "ts-first", now })
	next, _ := b.Incidents.Signal(rules.Signal{Kind: "odd", Key: "b", Host: "eps-rt-02", TplID: "dhcp2", At: now.Add(3 * time.Minute)})
	if th, why := b.relatedThread(next); th != "ts-first" || !strings.Contains(why, first.ID) {
		t.Fatalf("got %q %q", th, why)
	}
	other, _ := b.Incidents.Signal(rules.Signal{Kind: "odd", Key: "c", Host: "eps-rt-01", TplID: "bgp", At: now.Add(3 * time.Minute)})
	if th, _ := b.relatedThread(other); th != "" {
		t.Fatalf("another host joined: %q", th)
	}
}
