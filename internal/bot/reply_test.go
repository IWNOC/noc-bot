package bot

import (
	"strings"
	"testing"
)

func TestToMrkdwn(t *testing.T) {
	in := "### 状況\n**沈静化**しています。\n*   **Up**: 11:14\n- 詳細は [PeeringDB](https://www.peeringdb.com/net/11925)\n```\n**not touched** - here\n```"
	got := toMrkdwn(in)
	for _, want := range []string{"*状況*", "*沈静化*しています", "• *Up*: 11:14", "<https://www.peeringdb.com/net/11925|PeeringDB>", "**not touched** - here"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(strings.Split(got, "```")[0], "**") {
		t.Fatalf("markdown bold left:\n%s", got)
	}
}

func TestForeign(t *testing.T) {
	if f := foreign("上流の BGP セッションが周期的に落ちています。確認できていません。"); len(f) != 0 {
		t.Fatalf("plain Japanese flagged: %v", f)
	}
	if f := foreign("AS45679 相关的 BGP、无线とは無関係、10分마다、时刻"); len(f) < 4 {
		t.Fatalf("Chinese/Korean not caught: %v", f)
	}
}

func TestStripFooterAndOnlyMentions(t *testing.T) {
	s := stripFooter("答えです。\n_（確かめたこと: a, b）_\n_（確かめたこと: a）_")
	if s != "答えです。" {
		t.Fatalf("%q", s)
	}
	if !onlyMentions("<!subteam^S07LLMYUXT7>") || !onlyMentions("<@U1> <@U2>") || onlyMentions("<@U1> これ見て") || onlyMentions("") {
		t.Fatal("onlyMentions")
	}
}
