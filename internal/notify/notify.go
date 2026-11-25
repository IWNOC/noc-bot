// Package notify posts to Slack. Facts are written by code from the incident's exact values; the
// LLM's interpretation goes in a separate, marked section so a wrong guess never alters a number.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/IWNOC/noc-bot/internal/incident"
)

// Notifier posts a message and returns a thread id when the transport supports threads.
type Notifier interface {
	Post(ctx context.Context, text, thread string) (string, error)
}

// Log is a dry-run notifier.
type Log struct{ L *slog.Logger }

func (n Log) Post(_ context.Context, text, thread string) (string, error) {
	n.L.Info("slack (dry run)", "thread", thread, "text", text)
	return "", nil
}

// Webhook posts through an Incoming Webhook. It cannot thread: replies are posted top-level.
type Webhook struct {
	URL  string
	HTTP *http.Client
}

func (w Webhook) Post(ctx context.Context, text, thread string) (string, error) {
	b, _ := json.Marshal(map[string]any{"text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("slack webhook: HTTP %d: %s", resp.StatusCode, data)
	}
	return "", nil
}

func (w Webhook) client() *http.Client {
	if w.HTTP != nil {
		return w.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Bot posts with a bot token (chat.postMessage), which supports threads.
type Bot struct {
	Token, Channel string
	HTTP           *http.Client
}

func (b Bot) Post(ctx context.Context, text, thread string) (string, error) {
	body := map[string]any{"channel": b.Channel, "text": text, "unfurl_links": false}
	if thread != "" {
		body["thread_ts"] = thread
	}
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/chat.postMessage", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+b.Token)
	c := b.HTTP
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var r struct {
		OK    bool   `json:"ok"`
		TS    string `json:"ts"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	if !r.OK {
		return "", fmt.Errorf("slack: %s", r.Error)
	}
	return r.TS, nil
}

var kindTitle = map[string]string{
	"rate":       "ログの急増・停止",
	"silence":    "ログの途絶",
	"clock_skew": "時刻ずれ",
	"odd":        "いつもと違うログ",
	"window":     "全体のようすがいつもと違う",
}

var levelJA = map[string]string{
	"FAR ABOVE NORMAL": "普段より大幅に多い",
	"STOPPED":          "止まった",
	"NEW":              "初めて見る",
}

// Render builds the Slack text for an incident event. mention is the team's mention string, used
// only when the incident should page.
func Render(ev incident.Event, mention string) string {
	in := ev.Incident
	var b strings.Builder
	icon := map[incident.EventKind]string{incident.EvOpened: "🔴", incident.EvReminder: "🟠", incident.EvResolved: "✅"}[ev.Kind]
	title := kindTitle[in.Kind]
	if title == "" {
		title = in.Kind
	}
	switch ev.Kind {
	case incident.EvResolved:
		fmt.Fprintf(&b, "%s *解消* %s：%s（%s〜%s、%d回検知）\n", icon, title, in.Host,
			jst(in.Opened), jst(in.Resolved), in.Hits)
		return b.String()
	case incident.EvReminder:
		fmt.Fprintf(&b, "%s *継続中* %s：%s（%s から、%d回検知）\n", icon, title, in.Host, jst(in.Opened), in.Hits)
	default:
		fmt.Fprintf(&b, "%s *%s*：%s", icon, title, in.Host)
		if in.Page && mention != "" {
			b.WriteString(" " + mention)
		}
		b.WriteString("\n")
	}
	if in.Template != "" {
		fmt.Fprintf(&b, "パターン：`%s`\n", clip(in.Template, 300))
	}
	keys := make([]string, 0, len(in.Facts))
	for k := range in.Facts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := in.Facts[k]
		if k == "example" {
			fmt.Fprintf(&b, "例：```%s```\n", clip(v, 500))
			continue
		}
		if ja, ok := levelJA[v]; ok {
			v = v + "（" + ja + "）"
		}
		fmt.Fprintf(&b, "• %s：%s\n", k, v)
	}
	if in.Team != "" {
		fmt.Fprintf(&b, "担当：%s", in.Team)
		if in.Severity >= 0 {
			fmt.Fprintf(&b, "　重要度：%d/3", in.Severity)
		}
		b.WriteString("\n")
	}
	if in.Annotation != "" && ev.Kind == incident.EvOpened {
		b.WriteString("──────────\n🤖 *見立て*（AI。事実は上の数字を正としてください）\n")
		b.WriteString(clip(in.Annotation, 1200))
		b.WriteString("\n")
	}
	return b.String()
}

func jst(t time.Time) string {
	return t.In(time.FixedZone("JST", 9*3600)).Format("01/02 15:04")
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
