package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/IWNOC/noc-bot/internal/incident"
)

func TestRenderSeparatesFactsFromAI(t *testing.T) {
	in := incident.Incident{Kind: "rate", Host: "eps-rt-01", Template: "%BGP-3-NOTIFICATION <*>", Opened: time.Now(),
		Facts: map[string]string{"last_hour": "19", "normal_per_hour": "0.2"}, Team: "l2l3", Severity: 2, Page: true,
		Annotation: "v4 と v6 の両方で Hold Timer 切れ。"}
	s := Render(incident.Event{Kind: incident.EvOpened, Incident: in}, "<!subteam^S1>")
	for _, want := range []string{"<!subteam^S1>", "• 直近1時間の件数：19", "🤖 *見立て*", "担当：l2l3　重要度：2/3"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in\n%s", want, s)
		}
	}
	if strings.Index(s, "直近1時間") > strings.Index(s, "見立て") {
		t.Fatal("facts must come before the AI section")
	}
	in.Page = false
	if strings.Contains(Render(incident.Event{Kind: incident.EvOpened, Incident: in}, "<!subteam^S1>"), "subteam") {
		t.Fatal("no mention unless paging")
	}
}
