package slack

import (
	"regexp"
	"strings"
)

// FooterBlockID marks every message agent-shift posts, so the relay can recognise its own
// messages (they come from the user's account, like everything else the user writes).
const FooterBlockID = "agentshift_footer"

// MaxMarkdown is the per-message budget for markdown blocks (Slack allows 12,000 chars).
const MaxMarkdown = 11000

// Compose builds a message body: the text as a `markdown` block (standard Markdown, which
// is what LLMs write) plus a subtle context-block footer. fallback is the notification text.
func Compose(text, footer string) (fallback string, blocks []any) {
	blocks = []any{map[string]any{"type": "markdown", "text": text}}
	fallback = ToMrkdwn(text)
	if footer != "" {
		blocks = append(blocks, map[string]any{
			"type":     "context",
			"block_id": FooterBlockID,
			"elements": []any{map[string]any{"type": "mrkdwn", "text": footer}},
		})
		fallback += "\n\n_" + footer + "_"
	}
	return fallback, blocks
}

// Split breaks text into chunks of at most max bytes, preferring paragraph, then line boundaries.
func Split(text string, max int) []string {
	var out []string
	for len(text) > max {
		cut := strings.LastIndex(text[:max], "\n\n")
		if cut < max/3 {
			cut = strings.LastIndex(text[:max], "\n")
		}
		if cut < max/3 {
			cut = strings.LastIndex(text[:max], " ")
		}
		if cut <= 0 {
			cut = max
		}
		out = append(out, strings.TrimRight(text[:cut], " \n"))
		text = strings.TrimLeft(text[cut:], " \n")
	}
	if strings.TrimSpace(text) != "" || len(out) == 0 {
		out = append(out, text)
	}
	return out
}

var (
	mdBold    = regexp.MustCompile(`\*\*(.+?)\*\*`)
	mdStrike  = regexp.MustCompile(`~~(.+?)~~`)
	mdLink    = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^)\s]+)\)`)
	mdHeading = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
)

// ToMrkdwn converts common Markdown to Slack mrkdwn (used for fallback/notification text).
func ToMrkdwn(s string) string {
	s = mdHeading.ReplaceAllString(s, "*$1*")
	s = mdBold.ReplaceAllString(s, "*$1*")
	s = mdStrike.ReplaceAllString(s, "~$1~")
	s = mdLink.ReplaceAllString(s, "<$2|$1>")
	return s
}
