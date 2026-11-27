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
	footerRe  = regexp.MustCompile(`(?s)\n*_（確かめたこと[:：].*?）_`)
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
