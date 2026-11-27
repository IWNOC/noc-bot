//go:build live

package bot

// go test -tags live -run TestLiveAgent ./internal/bot  (needs LLM_URL, LLM_TOKEN)

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/IWNOC/noc-bot/internal/ingest"
	"github.com/IWNOC/noc-bot/internal/llm"
)

// A flap that has gone on all day at a steady rate: the agent must not call it "settled".
func TestLiveAgent(t *testing.T) {
	b, fs, _, _ := setup(t, "unused")
	b.Agent = llm.New(os.Getenv("LLM_URL"), os.Getenv("LLM_TOKEN"), "Qwen3.8-Flash-Next-oQ4e-fp16-mtp", 90*time.Second)
	b.LLM = b.Agent
	now := time.Now().UTC()
	for m := 20 * 60; m > 0; m -= 3 { // every 3 minutes for 20 hours
		at := now.Add(-time.Duration(m) * time.Minute)
		raw := fmt.Sprintf("%s 10.110.0.1 eps-rt-01 devtime=%s Bgp: %%BGP-5-ADJCHANGE: peer 163.220.192.86 (VRF default AS 45679) old state Established event RecvNotify new state Idle",
			at.Format(time.RFC3339), at.Add(9*time.Hour).Format(time.RFC3339))
		l, _ := ingest.Parse(raw)
		b.Handle(l)
		b.Stats.Tick(at.Add(time.Minute))
	}
	b.Posted("C1", "t1")
	ask := func(msg string) {
		n := len(fs.posts)
		start := time.Now()
		b.OnMention(context.Background(), "C1", "t1", "<@UBOT> "+msg, "U1")
		t.Logf("\n>>> %s\n<<< (%s)\n%s", msg, time.Since(start).Round(time.Second), strings.Join(fs.posts[n:], "\n---\n"))
	}
	ask("TOMOCHA-NET の BGP フラップってもう起きてない?")
	ask("あなたは誰？U1 って誰かわかる？")
}
