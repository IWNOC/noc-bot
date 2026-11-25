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

func setup(t *testing.T, llmReply string) (*Bot, *fakeSlack, *[]feedbackCall, incident.Incident) {
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
	lm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": llmReply}}}})
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
	reply := b.OnAnswer(context.Background(), in.ID, slackapp.ActWrong, "", "U1")
	if !strings.Contains(reply, "学習に登録") {
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

func TestCommentBecomesProposalAndOnlyAcceptedIsLearned(t *testing.T) {
	reply := `{"summary_ja":"eps-rt-02 の未使用 IF の DHCP は直さない","correct":null,"severity":null,"team":"",
	"known_issue":{"reason":"未使用 IF の DHCP クライアント。イベント中は直さない","scope":"template","days":2},"fact":"","resolution":"","instruction":""}`
	b, fs, calls, in := setup(t, reply)
	b.OnThreadReply(context.Background(), "ts1", "これは未使用の IF、イベント中は直さないので既知で", "U2")
	if len(fs.cards) != 1 {
		t.Fatalf("expected one proposal card, got %d", len(fs.cards))
	}
	// nothing learned before a person accepts
	for _, c := range *calls {
		if c.Desk != "comment" {
			t.Fatalf("learned before acceptance: %+v", c)
		}
	}
	var id, card string
	for k, v := range fs.cards {
		id, card = k, v
	}
	if !strings.Contains(card, "既知の問題として登録") {
		t.Fatalf("card %q", card)
	}
	out := b.OnProposal(context.Background(), id, true, "U3")
	if !strings.Contains(out, "既知の問題") {
		t.Fatalf("accept reply %q", out)
	}
	// the known issue now covers the template, and survives a restart via the store
	meta, _ := b.Rules.Template("tpl1")
	if b.Rules.Covering(meta, time.Now()) == nil {
		t.Fatal("known issue not applied")
	}
	b2, _ := New(b.Cfg, b.Log, b.Store, nil, nil, fs)
	b2.Rules.Observe(meta, true, time.Now(), 0)
	if b2.Rules.Covering(meta, time.Now()) == nil {
		t.Fatal("known issue not restored after restart")
	}
	// a second click on the same card does nothing
	if out := b.OnProposal(context.Background(), id, true, "U3"); !strings.Contains(out, "処理済み") {
		t.Fatalf("double apply: %q", out)
	}
	_ = in
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
func TestTeamSetupFromPlainHandles(t *testing.T) {
	reply := `{"intent":"team_setup","teams":[
	{"team":"l2l3","group":"team-l2l3","description":"ルーター・スイッチ・BGP","hosts":["eps-rt-*"]},
	{"team":"l1","group":"team-l1","description":"配線・光"},
	{"team":"server","group":"team-made-up","description":"x"}]}`
	b, fs, _, _ := setup(t, reply)
	b.OnMention(context.Background(), "C1", "ts9", "<@UBOT> チームは @team-l2l3 と @team-l1、それぞれ担当", "U1")
	if len(fs.cards) != 1 {
		t.Fatalf("cards %d posts %v", len(fs.cards), fs.posts)
	}
	var id, card string
	for k, v := range fs.cards {
		id, card = k, v
	}
	if strings.Contains(card, "made-up") {
		t.Fatal("a group that does not exist must be dropped")
	}
	b.OnProposal(context.Background(), id, true, "U1")
	if tm, _ := b.team("l2l3"); tm.Mention != "<!subteam^S07LLMYUXT7>" {
		t.Fatalf("l2l3 not applied: %+v", tm)
	}
	if tm, ok := b.team("l1"); !ok || tm.Mention != "<!subteam^S07LS37RWAG>" {
		t.Fatalf("l1 not added: %+v", tm)
	}
	if s := b.senderFor("", "eps-rt-02"); s.Team != "l2l3" {
		t.Fatalf("sender rule not applied: %+v", s)
	}
	if !strings.Contains(b.describeTeams(), "→ <!subteam^S07LS37RWAG>") {
		t.Fatal(b.describeTeams())
	}
}

func TestSettingsQuestionShowsTeams(t *testing.T) {
	b, fs, _, _ := setup(t, `{"intent":"show_teams","teams":[]}`)
	b.OnMention(context.Background(), "C1", "ts9", "<@UBOT> まだメンション未設定状態になってる？", "U1")
	if len(fs.posts) != 1 || !strings.Contains(fs.posts[0], "今の担当設定") {
		t.Fatalf("posts %v", fs.posts)
	}
}
