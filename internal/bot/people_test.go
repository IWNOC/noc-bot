package bot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IWNOC/noc-bot/internal/config"
	"github.com/IWNOC/noc-bot/internal/incident"
	"github.com/IWNOC/noc-bot/internal/laya"
	"github.com/IWNOC/noc-bot/internal/llm"
	"github.com/IWNOC/noc-bot/internal/rules"
	"github.com/IWNOC/noc-bot/internal/slackapp"
	"github.com/IWNOC/noc-bot/internal/statebuilder"
	"github.com/IWNOC/noc-bot/internal/store"
)

type fakeSlack struct {
	mu    sync.Mutex
	posts []string
	cards map[string]string // proposal id -> text
}

func (f *fakeSlack) Post(_ context.Context, text, thread string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, text)
	return "ts1", nil
}
func (f *fakeSlack) PostIn(ctx context.Context, _, thread, text string) error {
	_, err := f.Post(ctx, text, thread)
	return err
}
func (f *fakeSlack) PostProposalIn(ctx context.Context, _, thread, id, text string) error {
	return f.PostProposal(ctx, thread, id, text)
}
func (f *fakeSlack) PostProposal(_ context.Context, _, id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cards[id] = text
	return nil
}

type feedbackCall struct {
	Desk       string
	Answers    map[string]any
	Correction bool
	Origin     string
}

// script is a list of assistant messages the fake LLM returns in order (the last one repeats).
type script []map[string]any

func toolCall(name string, args map[string]any) map[string]any {
	a, _ := json.Marshal(args)
	return map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1", "type": "function",
		"function": map[string]any{"name": name, "arguments": string(a)}}}}
}

func say(text string) map[string]any { return map[string]any{"role": "assistant", "content": text} }

func setup(t *testing.T, llmReply string) (*Bot, *fakeSlack, *[]feedbackCall, incident.Incident) {
	return setupScript(t, script{say(llmReply)})
}

func setupScript(t *testing.T, sc script) (*Bot, *fakeSlack, *[]feedbackCall, incident.Incident) {
	t.Helper()
	var mu sync.Mutex
	calls := &[]feedbackCall{}
	ly := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/feedback") {
			var body struct {
				Answers    map[string]any
				Correction bool
				Origin     string
			}
			json.NewDecoder(r.Body).Decode(&body)
			desk := strings.Split(r.URL.Path, "/")[3]
			mu.Lock()
			*calls = append(*calls, feedbackCall{desk, body.Answers, body.Correction, body.Origin})
			mu.Unlock()
		}
		w.Write([]byte(`{"answers":{}}`))
	}))
	t.Cleanup(ly.Close)
	var n int
	lm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		m := sc[min(n, len(sc)-1)]
		n++
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": m}}})
	}))
	t.Cleanup(lm.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, _ := store.Open(filepath.Join(t.TempDir(), "p.db"))
	t.Cleanup(func() { st.Close() })
	fs := &fakeSlack{cards: map[string]string{}}
	cfg := &config.Config{Teams: map[string]config.Team{"l2l3": {}, "server": {}}}
	b, err := New(cfg, log, st, laya.New(ly.URL, "", 5*time.Second), llm.New(lm.URL, "", "m", 5*time.Second), fs)
	if err != nil {
		t.Fatal(err)
	}
	// an "odd" incident from the line desk, with both decisions on record
	sig := rules.Signal{Kind: "odd", Key: "odd:tpl1", Host: "eps-rt-02", Template: "No DHCPOFFERS received.", TplID: "tpl1", At: time.Now(), Facts: map[string]string{}}
	b.Rules.Observe(rules.TemplateMeta{ID: "tpl1", Text: "No DHCPOFFERS received.", Program: "dhclient", Host: "eps-rt-02"}, true, time.Now(), 0)
	in, _ := b.Incidents.Signal(sig)
	in, _ = b.Incidents.Update(in.Key, func(i *incident.Incident) { i.Thread = "ts1"; i.Severity = 1; i.Team = "l2l3" })
	ctx := context.Background()
	st.Decision(ctx, time.Now(), "line", statebuilder.Version, "tpl1", "line text", map[string]any{}, true)
	st.Decision(ctx, time.Now(), "incident", statebuilder.Version, in.ID, "incident text", map[string]any{}, true)
	return b, fs, calls, in
}

func TestWrongButtonTeachesBothDesks(t *testing.T) {
	b, _, calls, in := setup(t, "{}")
	reply, undo := b.OnAnswer(context.Background(), in.ID, slackapp.ActWrong, "", "U1")
	if !strings.Contains(reply, "誤検知として学習") || undo == "" {
		t.Fatalf("reply %q", reply)
	}
	if len(*calls) != 2 {
		t.Fatalf("feedback calls %+v", *calls)
	}
	for _, c := range *calls {
		if !c.Correction || c.Origin != "human" {
			t.Fatalf("must be a human correction: %+v", c)
		}
		if c.Desk == "line" && c.Answers["odd"] != false {
			t.Fatalf("line desk should learn odd=false: %+v", c)
		}
	}
}

func TestAgentProposesKnownIssueOnlyAppliedOnAccept(t *testing.T) {
	b, fs, calls, _ := setupScript(t, script{
		toolCall("propose_known_issue", map[string]any{"incident_id": "this", "reason": "未使用 IF の DHCP。イベント中は直さない", "scope": "template", "days": 2,
			"summary": "eps-rt-02 の DHCP の繰り返しを既知の問題にします"}),
		say("既知の問題として登録するカードを出しました。登録を押すと反映されます。"),
	})
	b.OnThreadReply(context.Background(), "C1", "ts1", "これは未使用の IF、イベント中は直さないので既知で", "U2")
	if len(fs.cards) != 1 {
		t.Fatalf("expected one card, got %d (posts %v)", len(fs.cards), fs.posts)
	}
	if len(fs.posts) == 0 || !strings.Contains(fs.posts[len(fs.posts)-1], "カードを出しました") {
		t.Fatalf("agent reply missing: %v", fs.posts)
	}
	meta, _ := b.Rules.Template("tpl1")
	if b.Rules.Covering(meta, time.Now()) != nil {
		t.Fatal("applied before acceptance")
	}
	for _, c := range *calls {
		if c.Desk != "comment" {
			t.Fatalf("learned before acceptance: %+v", c)
		}
	}
	var id string
	for k := range fs.cards {
		id = k
	}
	if out := b.OnProposal(context.Background(), id, true, "U3"); !strings.Contains(out, "登録しました") {
		t.Fatalf("accept: %q", out)
	}
	if b.Rules.Covering(meta, time.Now()) == nil {
		t.Fatal("known issue not applied")
	}
	b2, _ := New(b.Cfg, b.Log, b.Store, nil, nil, fs)
	b2.Rules.Observe(meta, true, time.Now(), 0)
	if b2.Rules.Covering(meta, time.Now()) == nil {
		t.Fatal("known issue not restored after restart")
	}
	if out := b.OnProposal(context.Background(), id, true, "U3"); !strings.Contains(out, "処理済み") {
		t.Fatalf("double apply: %q", out)
	}
}

func TestRepliesOutsideOurThreadsIgnored(t *testing.T) {
	b, fs, _, _ := setup(t, "hello")
	b.OnThreadReply(context.Background(), "C1", "someone-elses-thread", "hi", "U1")
	if len(fs.posts) != 0 {
		t.Fatalf("answered a thread it was not in: %v", fs.posts)
	}
	b.Posted("C1", "our-thread")
	b.OnThreadReply(context.Background(), "C1", "our-thread", "hi", "U1")
	if len(fs.posts) != 1 {
		t.Fatalf("did not answer in its own thread: %v", fs.posts)
	}
}

func TestMemoryProposalAndRecall(t *testing.T) {
	b, fs, _, _ := setupScript(t, script{
		toolCall("propose_memory", map[string]any{"fact": "eps-rt-01 は AS45679 につながる境界ルーター", "summary": "eps-rt-01 の役割を覚えます"}),
		say("覚えるカードを出しました。"),
	})
	b.Posted("C1", "t9")
	b.OnMention(context.Background(), "C1", "t9", "<@UBOT> eps-rt-01 は境界ルーターで上流は AS45679", "U1")
	var id string
	for k := range fs.cards {
		id = k
	}
	if id == "" {
		t.Fatal("no card")
	}
	b.OnProposal(context.Background(), id, true, "U1")
	if got := b.memoryAbout(context.Background(), "eps-rt-01"); len(got) != 1 {
		t.Fatalf("memory not recalled: %v", got)
	}
	if !strings.Contains(b.agentContext(context.Background(), nil), "境界ルーター") {
		t.Fatal("memory not in the agent's context")
	}
}

func TestTemplateRegexp(t *testing.T) {
	re := regexp.MustCompile(templateRegexp("DHCPDISCOVER on <*> to <IP> port <NUM> interval <NUM>"))
	if !re.MatchString("DHCPDISCOVER on <*> to <IP> port <NUM> interval <NUM>") {
		t.Fatal("must match its own template")
	}
	if re.MatchString("DHCPOFFER on eth2") {
		t.Fatal("too broad")
	}
}

func (f *fakeSlack) Groups(context.Context) ([]slackapp.Group, error) {
	return []slackapp.Group{{ID: "S07LLMYUXT7", Handle: "team-l2l3"}, {ID: "S07LGV7MM4N", Handle: "team-server"}, {ID: "S07LS37RWAG", Handle: "team-l1"}}, nil
}

// Plain-text "@team-l2l3" (not a real mention) still resolves through the workspace's groups;
// a group that does not exist is dropped.
// A team must be a real Slack user group; one the model made up is refused before any card.
func TestTeamProposalNeedsRealGroup(t *testing.T) {
	b, fs, _, _ := setupScript(t, script{
		toolCall("propose_team", map[string]any{"group": "team-made-up", "owns": "x", "summary": "x"}),
		toolCall("propose_team", map[string]any{"group": "@team-l1", "owns": "配線・光・電源", "hosts": []string{"pdu-*"}, "summary": "team-l1 を物理層の担当にします"}),
		say("担当のカードを出しました。"),
	})
	b.Posted("C1", "t1")
	b.OnMention(context.Background(), "C1", "t1", "<@UBOT> 配線とか光は team-l1", "U1")
	if len(fs.cards) != 1 {
		t.Fatalf("cards %d", len(fs.cards))
	}
	var id, card string
	for k, v := range fs.cards {
		id, card = k, v
	}
	if !strings.Contains(card, "<!subteam^S07LS37RWAG>") {
		t.Fatalf("card %q", card)
	}
	b.OnProposal(context.Background(), id, true, "U1")
	if tm, ok := b.team("team-l1"); !ok || tm.Mention != "<!subteam^S07LS37RWAG>" {
		t.Fatalf("team not applied: %+v", tm)
	}
	if s := b.senderFor("", "pdu-3"); s.Team != "team-l1" {
		t.Fatalf("host rule not applied: %+v", s)
	}
}

// A held learning batch is shown once, and the person's choice reaches laya.
func TestHeldBatchSurfacedAndResolved(t *testing.T) {
	var got []string
	ly := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/incident/held"):
			w.Write([]byte(`{"area":"the heads agree with this batch 17% of the time"}`))
		case strings.Contains(r.URL.Path, "/held/"):
			got = append(got, r.URL.Path)
			w.Write([]byte(`{}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer ly.Close()
	b, fs, _, _ := setup(t, "{}")
	b.Laya = laya.New(ly.URL, "", 5*time.Second)
	b.checkHeld(context.Background())
	b.checkHeld(context.Background()) // not shown twice
	if len(fs.cards) != 1 {
		t.Fatalf("cards %d", len(fs.cards))
	}
	for id, card := range fs.cards {
		if !strings.Contains(card, "食い違って") {
			t.Fatalf("card %q", card)
		}
		if out := b.OnProposal(context.Background(), id, false, "U1"); !strings.Contains(out, "破棄") {
			t.Fatalf("%q", out)
		}
	}
	if len(got) != 1 || !strings.HasSuffix(got[0], "/incident/held/area/discard") {
		t.Fatalf("laya calls %v", got)
	}
}
