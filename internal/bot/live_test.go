//go:build live

package bot

// go test -tags live -run TestLiveAgent ./internal/bot  (needs LLM_URL, LLM_TOKEN)

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/IWNOC/noc-bot/internal/llm"
)

func TestLiveAgent(t *testing.T) {
	b, fs, _, _ := setup(t, "unused")
	b.Agent = llm.New(os.Getenv("LLM_URL"), os.Getenv("LLM_TOKEN"), "Qwen3.8-Flash-Next-oQ4e-fp16-mtp", 90*time.Second)
	b.LLM = b.Agent
	b.Posted("C1", "t1")
	ask := func(msg string) {
		n := len(fs.posts)
		start := time.Now()
		b.OnMention(context.Background(), "C1", "t1", "<@UBOT> "+msg, "U1")
		t.Logf("\n>>> %s\n<<< (%s)\n%s\ncards: %d", msg, time.Since(start).Round(time.Second), strings.Join(fs.posts[n:], "\n---\n"), len(fs.cards))
		for id, c := range fs.cards {
			t.Logf("CARD %s:\n%s", id, c)
		}
	}
	ask("今ね。チーム構成として、team-server,team-ap,team-l2l3,team-l1がいる。それぞれ最初に@をつける必要がある")
	ask("まだメンション未設定状態になってる？")
	ask("いま何か気になることある？")
}
