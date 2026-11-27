package bot

// Making what the LLM writes fit Slack and its readers. Found by reading the event's threads:
// replies used Markdown that Slack does not render (**bold**, ### headings), slipped into Chinese
// and Korean words mid-sentence, repeated their own footer, and called people by raw user ids.

import (
	"context"
	"regexp"
	"strings"
	"time"

	"golang.org/x/text/encoding/japanese"

	"github.com/IWNOC/noc-bot/internal/llm"
)

var (
	mdBold    = regexp.MustCompile(`\*\*([^*\n]+?)\*\*`)
	mdHeading = regexp.MustCompile(`(?m)^\s{0,3}#{1,6}\s+(.+?)\s*#*\s*$`)
	mdLink    = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)`)
	mdBullet  = regexp.MustCompile(`(?m)^(\s*)[*-]\s+`)
	footerRe  = regexp.MustCompile(`(?s)\n*_（(?:確かめたこと|確認)[:：].*?）_`)
)

// toMrkdwn turns the Markdown models write into Slack's mrkdwn. Code blocks are left alone.
func toMrkdwn(s string) string {
	parts := strings.Split(s, "```")
	for i := 0; i < len(parts); i += 2 {
		p := parts[i]
		p = mdBullet.ReplaceAllString(p, "$1• ")
		p = mdHeading.ReplaceAllString(p, "*$1*")
		p = mdBold.ReplaceAllString(p, "*$1*")
		p = mdLink.ReplaceAllString(p, "<$2|$1>")
		parts[i] = p
	}
	return strings.Join(parts, "```")
}

// stripFooter removes the "what I checked" footer (the model copies it from earlier turns).
func stripFooter(s string) string { return strings.TrimSpace(footerRe.ReplaceAllString(s, "")) }

// foreign lists Hangul and Chinese-only characters in s: ideographs that are not in the Japanese
// character set (简体字 such as 关 无 时), which is how a slip into Chinese shows up.
func foreign(s string) []string {
	enc := japanese.ShiftJIS.NewEncoder()
	seen := map[rune]bool{}
	var out []string
	for _, r := range s {
		hangul := (r >= 0xAC00 && r <= 0xD7A3) || (r >= 0x1100 && r <= 0x11FF) || (r >= 0x3130 && r <= 0x318F)
		han := r >= 0x4E00 && r <= 0x9FFF
		if !hangul && !han || seen[r] {
			continue
		}
		if han {
			if _, err := enc.String(string(r)); err == nil {
				continue
			}
			enc.Reset()
		}
		seen[r] = true
		out = append(out, string(r))
	}
	return out
}

const japaneseOnly = `次の文章には中国語や韓国語の語が混ざっています。意味と構成を変えずに、自然な日本語だけに直してください。
数字、IP アドレス、時刻、ID（<@U…> や tpl:… など）、URL、コード、ログの引用は一字も変えないこと。直した文章だけを返してください。`

// inJapanese rewrites a text once if it slipped into another language. On any failure the
// original is returned: a mixed-language answer is better than none.
func (b *Bot) inJapanese(ctx context.Context, s string) string {
	if len(foreign(s)) == 0 || b.agentLLM() == nil {
		return s
	}
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	m, err := b.agentLLM().Chat(cctx, []llm.Message{{Role: "system", Content: japaneseOnly}, {Role: "user", Content: s}}, nil, 1800, false)
	out := strings.TrimSpace(m.Content)
	if err != nil || len([]rune(out)) < len([]rune(s))/2 || len(foreign(out)) >= len(foreign(s)) {
		return s
	}
	return out
}

// Identity is what the Slack side knows about who is who.
type Identity interface {
	BotUserID() string
	UserName(ctx context.Context, id string) string
}

var userRef = regexp.MustCompile(`<@(U[A-Z0-9]+)>`)

// onlyMentions reports whether a message is nothing but mentions of people or groups: someone
// calling a colleague into the thread, not talking to noc-bot.
func onlyMentions(text string) bool {
	t := regexp.MustCompile(`<[@!][^>]+>`).ReplaceAllString(text, "")
	return strings.TrimSpace(t) == "" && strings.Contains(text, "<")
}

// sourceWords says what was looked at in people's words, not tool names.
func sourceWords(tools []string) string {
	names := map[string]string{
		"open_incidents": "通知の一覧", "incident_detail": "通知の一覧",
		"sender_overview": "ログ", "patterns_of_host": "ログ", "recent_lines": "ログ", "pattern_history": "ログの履歴", "pattern_detail": "ログ",
		"radius_summary":  "RADIUS の集計",
		"zabbix_problems": "Zabbix", "zabbix_items": "Zabbix",
		"web_search": "Web", "fetch_url": "Web", "lookup_network": "AS・アドレスの登録情報",
		"noc_settings": "自分の設定と記憶", "site_notes": "自分の設定と記憶", "known_issues": "既知の問題", "notebook_read": "調査ノート",
		"slack_groups": "Slack のグループ",
	}
	var out []string
	seen := map[string]bool{}
	for _, t := range tools {
		if n, ok := names[t]; ok && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return strings.Join(out, "、")
}

var impactWords = []string{"なし", "小（一部の人）", "中（多くの人）", "大（全体・中核）"}

// friendlyName is what to call an alert when talking to people: its headline if it has one,
// else what the investigator learned the message means, else the raw pattern.
func (b *Bot) friendlyName(ctx context.Context, title, tplID, template string, n int) string {
	if title != "" {
		return clipText(title, n)
	}
	if m, _ := b.background(ctx, tplID, ""); m != "" {
		first := m
		for _, sep := range []string{"。", ". ", "\n"} {
			if i := strings.Index(first, sep); i > 0 {
				first = first[:i]
			}
		}
		return clipText(strings.Trim(first, "\"「」 "), n)
	}
	return clipText(template, n)
}

var (
	emailRe   = regexp.MustCompile(`[\w.+-]+@[\w-]+(?:\.[\w-]+)+`)
	macRe     = regexp.MustCompile(`(?i)\b((?:[0-9a-f]{2}[:-]){3})(?:[0-9a-f]{2}[:-]){2}[0-9a-f]{2}\b`)
	tplRefRe  = regexp.MustCompile(`\s*[（(]?tpl:[0-9a-f@.\w-]+[)）]?`)
	levelRefs = strings.NewReplacer("FAR ABOVE NORMAL", "いつもよりかなり多い", "ABOVE NORMAL", "いつもより多め", "BELOW NORMAL", "いつもより少なめ")
)

// forChannel makes investigator text fit a channel everyone reads: no account names, no full
// client MAC addresses, no internal ids or level names.
func forChannel(s string) string {
	s = emailRe.ReplaceAllString(s, "（利用者）")
	s = macRe.ReplaceAllString(s, "${1}…")
	s = tplRefRe.ReplaceAllString(s, "")
	return levelRefs.Replace(s)
}

// firstSentences keeps whole sentences up to about n characters.
func firstSentences(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	cut := -1
	for i := n; i > n/2; i-- {
		if r[i] == '。' || r[i] == '\n' {
			cut = i + 1
			break
		}
	}
	if cut < 0 {
		return string(r[:n]) + "…"
	}
	return strings.TrimSpace(string(r[:cut]))
}
