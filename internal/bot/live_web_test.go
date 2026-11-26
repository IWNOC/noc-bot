//go:build live

package bot

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/IWNOC/noc-bot/internal/llm"
	"github.com/IWNOC/noc-bot/internal/web"
)

func TestLiveAgentLookup(t *testing.T) {
	b, fs, _, _ := setup(t, "unused")
	b.Agent = llm.New(os.Getenv("LLM_URL"), os.Getenv("LLM_TOKEN"), "Qwen3.8-Flash-Next-oQ4e-fp16-mtp", 90*time.Second)
	b.LLM = b.Agent
	b.Web = web.New(os.Getenv("SEARX_URL"))
	b.Posted("C1", "t1")
	start := time.Now()
	b.OnMention(context.Background(), "C1", "t1", "<@UBOT> eps-rt-01 の BGP で BAD_AS_NUMBER で拒否してる相手 2401:5e40:c600::15:1371:1 って誰？AS45679 もどこの組織？", "U1")
	t.Logf("(%s)\n%s", time.Since(start).Round(time.Second), strings.Join(fs.posts, "\n---\n"))
}
