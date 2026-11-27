//go:build live

package bot

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/IWNOC/noc-bot/internal/llm"
	"github.com/IWNOC/noc-bot/internal/zabbix"
)

func TestLiveStatus(t *testing.T) {
	b, fs, _, _ := setup(t, "unused")
	b.Agent = llm.New(os.Getenv("LLM_URL"), os.Getenv("LLM_TOKEN"), "Qwen3.8-Flash-Next-oQ4e-fp16-mtp", 90*time.Second)
	b.LLM = b.Agent
	b.Zabbix = zabbix.New("https://10.20.4.5/zabbix", os.Getenv("ZBX"), true)
	b.Posted("C1", "t1")
	for _, q := range []string{"今どうなってる？", "いま端末は何台くらいつながってる？"} {
		n := len(fs.posts)
		st := time.Now()
		b.OnMention(context.Background(), "C1", "t1", "<@UBOT> "+q, "U1")
		t.Logf("\n>>> %s (%s)\n%s", q, time.Since(st).Round(time.Second), strings.Join(fs.posts[n:], "\n---\n"))
	}
}
