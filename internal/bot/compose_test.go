package bot

import (
	"context"
	"strings"
	"testing"
)

func TestUngrounded(t *testing.T) {
	ev := `peer 163.220.192.86 AS 45679 last_hour 19 at 18:26:56 v6 2401:5e40:c600::4:5679:1`
	if bad := ungrounded("AS45679 の 163.220.192.86 が 1時間に 19 回、18:26:56 にも", ev); len(bad) != 0 {
		t.Fatalf("grounded values flagged: %v", bad)
	}
	if bad := ungrounded("1時間に 25 回、相手は 163.220.192.87", ev); len(bad) != 2 {
		t.Fatalf("made-up values not caught: %v", bad)
	}
	if bad := ungrounded("重要度 2/3、手順 1 と 2", ev); len(bad) != 0 {
		t.Fatalf("small prose numbers should pass: %v", bad)
	}
}

// A composed alert with a number the LLM made up is not used.
func TestComposeDropsUngroundedText(t *testing.T) {
	b, _, _, in := setupScript(t, script{say(`{"title":"eps-rt-02 で DHCP が 120 回失敗","body":"上流を確認"}`)})
	if _, _, _, ok := b.compose(context.Background(), in); ok {
		t.Fatal("120 is not in the evidence; the text must be dropped")
	}
	b2, _, _, in2 := setupScript(t, script{say(`{"title":"eps-rt-02 の DHCP が応答なし","body":"未使用の IF かもしれません（要確認）"}`)})
	title, body, _, ok := b2.compose(context.Background(), in2)
	if !ok || !strings.Contains(title, "DHCP") || body == "" {
		t.Fatalf("grounded text rejected: %q %q %v", title, body, ok)
	}
}
