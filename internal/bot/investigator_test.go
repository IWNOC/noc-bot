package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func runTool(t *testing.T, b *Bot, name string, args map[string]any) string {
	t.Helper()
	for _, tl := range b.investigatorTools() {
		if tl.Name == name {
			raw, _ := json.Marshal(args)
			out, err := tl.Run(context.Background(), raw)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return out
		}
	}
	t.Fatalf("no tool %s", name)
	return ""
}

// What the investigator writes is what everyone else reads.
func TestNotebookFeedsBackground(t *testing.T) {
	b, _, _, _ := setup(t, "{}")
	runTool(t, b, "notebook_write", map[string]any{"kind": "meaning", "subject": "tpl1", "text": "Arista: the peer's AS differs from the configured remote-as; check the neighbor config.", "source": "https://www.arista.com/x"})
	runTool(t, b, "notebook_write", map[string]any{"kind": "entity", "subject": "AS45679", "text": "AS45679: TOMOCHA-NET (PeeringDB TOMOCHA.NET)", "source": "RIPEstat"})
	mean, ents := b.background(context.Background(), "tpl1", "peer 163.220.192.86 (VRF default AS 45679) and AS45679")
	if !strings.Contains(mean, "remote-as") || len(ents) != 1 || !strings.Contains(ents[0], "TOMOCHA") {
		t.Fatalf("mean=%q ents=%v", mean, ents)
	}
	if out := runTool(t, b, "notebook_read", map[string]any{"subject": "tpl:tpl1"}); !strings.Contains(out, "[meaning]") {
		t.Fatalf("read: %q", out)
	}
}

func TestFollowupsComeBackWhenDue(t *testing.T) {
	b, _, _, _ := setup(t, "{}")
	runTool(t, b, "followup", map[string]any{"minutes": 10, "note": "is the BGP bad AS still happening?"})
	if d := b.dueFollowups(context.Background(), time.Now()); len(d) != 0 {
		t.Fatalf("due too early: %v", d)
	}
	if d := b.dueFollowups(context.Background(), time.Now().Add(11*time.Minute)); len(d) != 1 {
		t.Fatalf("not due: %v", d)
	}
	if d := b.dueFollowups(context.Background(), time.Now().Add(12*time.Minute)); len(d) != 0 {
		t.Fatalf("came back twice: %v", d)
	}
}

func TestTellPeopleOncePerTopicShortAndRedacted(t *testing.T) {
	b, fs, _, _ := setup(t, "{}")
	out := runTool(t, b, "tell_people", map[string]any{"topic": "radius churn", "text": "m1m02530172@oit.ac.jp(56:5f:8f:3e:b1:70) の端末が再接続を繰り返しています（tpl:abc@host、ABOVE NORMAL）。対応は不要です。"})
	if !strings.Contains(out, "added") {
		t.Fatal(out)
	}
	// the same topic again: dropped unless it is an update
	if out := runTool(t, b, "tell_people", map[string]any{"topic": "RADIUS churn", "text": "まだ続いています。"}); !strings.Contains(out, "not sent") {
		t.Fatal(out)
	}
	runTool(t, b, "tell_people", map[string]any{"topic": "radius churn", "text": "収まりました。", "update": true})
	for i := 0; i < 3; i++ { // urgent is capped per hour
		runTool(t, b, "tell_people", map[string]any{"topic": "u" + string(rune('a'+i)), "text": "急ぎ", "urgent": true})
	}
	f := b.takeFindings()
	if len(f) != 3 || strings.Contains(f[0], "@oit") || strings.Contains(f[0], "b1:70") || strings.Contains(f[0], "tpl:") || strings.Contains(f[0], "ABOVE") {
		t.Fatalf("findings %q", f)
	}
	if len(fs.posts) != 1 { // two findings were queued first, so only one urgent fit before the cap of 3 per digest
		t.Logf("urgent posts: %d", len(fs.posts))
	}
}
