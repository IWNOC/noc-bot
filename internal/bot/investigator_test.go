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

func TestTellPeopleGoesToDigestAndUrgentIsCapped(t *testing.T) {
	b, fs, _, _ := setup(t, "{}")
	runTool(t, b, "tell_people", map[string]any{"text": "BGP の相手は AS45679"})
	for i := 0; i < 3; i++ {
		runTool(t, b, "tell_people", map[string]any{"text": "急ぎ", "urgent": true})
	}
	if len(fs.posts) != urgentPerHour {
		t.Fatalf("urgent posts %d", len(fs.posts))
	}
	if f := b.takeFindings(); len(f) != 2 { // one normal + one urgent over the cap
		t.Fatalf("findings %v", f)
	}
}
