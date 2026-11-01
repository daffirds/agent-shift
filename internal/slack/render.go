package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Resolver turns IDs into names while rendering. Implementations should be cached and best-effort.
type Resolver interface {
	UserName(ctx context.Context, id string) string
	ConversationName(ctx context.Context, id string) string
}

// Render converts a Slack message (text, Block Kit blocks, attachments, files) into
// readable Markdown-ish plain text for an LLM. User mentions keep their ID: "@Ana (U123)".
func Render(ctx context.Context, r Resolver, m *Message) string {
	rd := renderer{ctx: ctx, r: r}
	var parts []string

	blocks := rd.blocks(m.Blocks)
	text := rd.mrkdwn(m.Text)
	switch {
	case strings.TrimSpace(blocks) == "":
		parts = append(parts, text)
	case hasRichText(m.Blocks) || strings.TrimSpace(text) == "":
		parts = append(parts, blocks)
	default:
		// Block-only layouts (workflows, apps) often put details in blocks and a summary in text.
		if strings.Contains(normalizeSpace(blocks), normalizeSpace(text)) {
			parts = append(parts, blocks)
		} else {
			parts = append(parts, text, blocks)
		}
	}
	for _, a := range m.Attachments {
		if s := rd.attachment(a); s != "" {
			parts = append(parts, s)
		}
	}
	if len(m.Files) > 0 {
		var fl []string
		for _, f := range m.Files {
			fl = append(fl, "- "+DescribeFile(f))
		}
		parts = append(parts, "Attached files:\n"+strings.Join(fl, "\n"))
	}
	return strings.TrimSpace(joinNonEmpty(parts, "\n\n"))
}

// DescribeFile is a one-line summary of a file for prompts.
func DescribeFile(f File) string {
	name := f.Name
	if name == "" {
		name = f.Title
	}
	s := fmt.Sprintf("%s %q", f.ID, name)
	var meta []string
	if f.Mimetype != "" {
		meta = append(meta, f.Mimetype)
	}
	if f.Size > 0 {
		meta = append(meta, HumanSize(f.Size))
	}
	if len(meta) > 0 {
		s += " (" + strings.Join(meta, ", ") + ")"
	}
	return s
}

func HumanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

type renderer struct {
	ctx context.Context
	r   Resolver
}

func (rd renderer) user(id string) string {
	if rd.r == nil {
		return "@" + id
	}
	name := rd.r.UserName(rd.ctx, id)
	if name == "" || name == id {
		return "@" + id
	}
	return fmt.Sprintf("@%s (%s)", name, id)
}

func (rd renderer) channel(id, fallback string) string {
	if fallback != "" {
		return "#" + fallback
	}
	if rd.r != nil {
		if n := rd.r.ConversationName(rd.ctx, id); n != "" {
			return n
		}
	}
	return "#" + id
}

var mrkdwnToken = regexp.MustCompile(`<([^<>]+)>`)

// mrkdwn converts Slack mrkdwn escapes (<@U1>, <#C1|x>, <url|label>, <!here>) to plain text.
func (rd renderer) mrkdwn(s string) string {
	if s == "" {
		return ""
	}
	out := mrkdwnToken.ReplaceAllStringFunc(s, func(tok string) string {
		inner := tok[1 : len(tok)-1]
		target, label, _ := strings.Cut(inner, "|")
		switch {
		case strings.HasPrefix(target, "@"):
			return rd.user(target[1:])
		case strings.HasPrefix(target, "#"):
			return rd.channel(target[1:], label)
		case strings.HasPrefix(target, "!subteam^"):
			if label != "" {
				return label
			}
			return "@" + strings.TrimPrefix(target, "!subteam^")
		case strings.HasPrefix(target, "!date^"):
			if label != "" {
				return label
			}
			return target
		case strings.HasPrefix(target, "!"):
			return "@" + strings.TrimPrefix(target, "!")
		case label != "" && label != target && !strings.HasSuffix(target, label):
			return fmt.Sprintf("[%s](%s)", label, target)
		default:
			return strings.TrimPrefix(target, "mailto:")
		}
	})
	return unescape(out)
}

func unescape(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
}

// ---- Block Kit ----

type block struct {
	Type      string            `json:"type"`
	BlockID   string            `json:"block_id"`
	Text      json.RawMessage   `json:"text"`
	Fields    []textObject      `json:"fields"`
	Elements  []json.RawMessage `json:"elements"`
	Accessory json.RawMessage   `json:"accessory"`
	AltText   string            `json:"alt_text"`
	ImageURL  string            `json:"image_url"`
	Title     json.RawMessage   `json:"title"`
	Label     json.RawMessage   `json:"label"`
	Rows      json.RawMessage   `json:"rows"`
}

type textObject struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func hasRichText(raw []json.RawMessage) bool {
	for _, b := range raw {
		var h struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(b, &h) == nil && (h.Type == "rich_text" || h.Type == "markdown") {
			return true
		}
	}
	return false
}

func (rd renderer) blocks(raw []json.RawMessage) string {
	var out []string
	for _, r := range raw {
		var b block
		if json.Unmarshal(r, &b) != nil {
			continue
		}
		if s := strings.TrimRight(rd.block(b, r), "\n "); s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, "\n")
}

func (rd renderer) textObj(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var t textObject
	if json.Unmarshal(raw, &t) == nil && t.Text != "" {
		if t.Type == "mrkdwn" {
			return rd.mrkdwn(t.Text)
		}
		return t.Text
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s // markdown block: "text" is a plain string
	}
	return ""
}

func (rd renderer) block(b block, raw json.RawMessage) string {
	switch b.Type {
	case "rich_text":
		var sb strings.Builder
		for _, e := range b.Elements {
			rd.richBlock(&sb, e)
		}
		return sb.String()
	case "markdown":
		return rd.textObj(b.Text)
	case "header":
		return "## " + rd.textObj(b.Text)
	case "section":
		var lines []string
		if t := rd.textObj(b.Text); t != "" {
			lines = append(lines, t)
		}
		for _, f := range b.Fields {
			if f.Type == "mrkdwn" {
				lines = append(lines, rd.mrkdwn(f.Text))
			} else {
				lines = append(lines, f.Text)
			}
		}
		if s := rd.element(b.Accessory); s != "" {
			lines = append(lines, s)
		}
		return strings.Join(lines, "\n")
	case "context":
		var parts []string
		for _, e := range b.Elements {
			if s := rd.element(e); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	case "divider":
		return "---"
	case "image":
		s := "[image"
		if t := rd.textObj(b.Title); t != "" {
			s += ": " + t
		} else if b.AltText != "" {
			s += ": " + b.AltText
		}
		if b.ImageURL != "" {
			s += " " + b.ImageURL
		}
		return s + "]"
	case "actions":
		var parts []string
		for _, e := range b.Elements {
			if s := rd.element(e); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " | ")
	case "input":
		return rd.textObj(b.Label)
	default:
		return rd.anyText(raw)
	}
}

// element renders block elements (context items, accessories, buttons).
func (rd renderer) element(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var e struct {
		Type    string          `json:"type"`
		Text    json.RawMessage `json:"text"`
		AltText string          `json:"alt_text"`
		URL     string          `json:"url"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return ""
	}
	switch e.Type {
	case "mrkdwn", "plain_text":
		return rd.textObj(raw)
	case "image":
		if e.AltText != "" {
			return "[" + e.AltText + "]"
		}
		return ""
	case "button":
		label := rd.textObj(e.Text)
		if e.URL != "" {
			return fmt.Sprintf("[%s](%s)", label, e.URL)
		}
		return "[" + label + "]"
	default:
		return rd.textObj(e.Text)
	}
}

func (rd renderer) richBlock(sb *strings.Builder, raw json.RawMessage) {
	var e struct {
		Type     string            `json:"type"`
		Style    string            `json:"style"`
		Indent   int               `json:"indent"`
		Elements []json.RawMessage `json:"elements"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	switch e.Type {
	case "rich_text_section":
		sb.WriteString(rd.inline(e.Elements))
		sb.WriteString("\n")
	case "rich_text_list":
		for i, item := range e.Elements {
			var sec struct {
				Elements []json.RawMessage `json:"elements"`
			}
			_ = json.Unmarshal(item, &sec)
			marker := "- "
			if e.Style == "ordered" {
				marker = strconv.Itoa(i+1) + ". "
			}
			sb.WriteString(strings.Repeat("  ", e.Indent) + marker + rd.inline(sec.Elements) + "\n")
		}
	case "rich_text_quote":
		for _, line := range strings.Split(strings.TrimRight(rd.inline(e.Elements), "\n"), "\n") {
			sb.WriteString("> " + line + "\n")
		}
	case "rich_text_preformatted":
		sb.WriteString("```\n" + strings.TrimRight(rd.inline(e.Elements), "\n") + "\n```\n")
	default:
		sb.WriteString(rd.inline(e.Elements))
	}
}

func (rd renderer) inline(elems []json.RawMessage) string {
	var sb strings.Builder
	for _, raw := range elems {
		var e struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			URL       string `json:"url"`
			UserID    string `json:"user_id"`
			ChannelID string `json:"channel_id"`
			GroupID   string `json:"usergroup_id"`
			Name      string `json:"name"`
			Unicode   string `json:"unicode"`
			Range     string `json:"range"`
			Timestamp int64  `json:"timestamp"`
			Fallback  string `json:"fallback"`
			Style     struct {
				Bold   bool `json:"bold"`
				Italic bool `json:"italic"`
				Strike bool `json:"strike"`
				Code   bool `json:"code"`
			} `json:"style"`
		}
		if json.Unmarshal(raw, &e) != nil {
			continue
		}
		switch e.Type {
		case "text":
			t := e.Text
			if strings.TrimSpace(t) != "" {
				if e.Style.Code {
					t = "`" + t + "`"
				}
				if e.Style.Bold {
					t = "**" + t + "**"
				}
				if e.Style.Italic {
					t = "_" + t + "_"
				}
				if e.Style.Strike {
					t = "~~" + t + "~~"
				}
			}
			sb.WriteString(t)
		case "link":
			if e.Text != "" && e.Text != e.URL {
				sb.WriteString(fmt.Sprintf("[%s](%s)", e.Text, e.URL))
			} else {
				sb.WriteString(e.URL)
			}
		case "user":
			sb.WriteString(rd.user(e.UserID))
		case "channel":
			sb.WriteString(rd.channel(e.ChannelID, ""))
		case "usergroup":
			sb.WriteString("@" + e.GroupID)
		case "broadcast":
			sb.WriteString("@" + e.Range)
		case "emoji":
			if e.Unicode != "" {
				if r := emojiFromHex(e.Unicode); r != "" {
					sb.WriteString(r)
					continue
				}
			}
			sb.WriteString(":" + e.Name + ":")
		case "date":
			if e.Fallback != "" {
				sb.WriteString(e.Fallback)
			} else {
				sb.WriteString(time.Unix(e.Timestamp, 0).Format(time.RFC1123))
			}
		case "rich_text_section", "rich_text_list", "rich_text_quote", "rich_text_preformatted":
			rd.richBlock(&sb, raw)
		default:
			sb.WriteString(e.Text)
		}
	}
	return sb.String()
}

func emojiFromHex(hex string) string {
	var sb strings.Builder
	for _, part := range strings.Split(hex, "-") {
		v, err := strconv.ParseUint(part, 16, 32)
		if err != nil {
			return ""
		}
		sb.WriteRune(rune(v))
	}
	return sb.String()
}

// anyText extracts every "text" string from an unknown block, best-effort.
func (rd renderer) anyText(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	var out []string
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			if s, ok := t["text"].(string); ok && s != "" {
				out = append(out, rd.mrkdwn(s))
			}
			for k, c := range t {
				if k != "text" {
					walk(c)
				}
			}
		case []any:
			for _, c := range t {
				walk(c)
			}
		}
	}
	walk(v)
	return strings.Join(out, " ")
}

func (rd renderer) attachment(a Attachment) string {
	var lines []string
	if a.Pretext != "" {
		lines = append(lines, rd.mrkdwn(a.Pretext))
	}
	if a.AuthorName != "" {
		lines = append(lines, a.AuthorName+":")
	}
	if a.Title != "" {
		if a.TitleLink != "" {
			lines = append(lines, fmt.Sprintf("[%s](%s)", a.Title, a.TitleLink))
		} else {
			lines = append(lines, a.Title)
		}
	}
	if a.Text != "" {
		lines = append(lines, rd.mrkdwn(a.Text))
	}
	for _, f := range a.Fields {
		lines = append(lines, fmt.Sprintf("%s: %s", f.Title, rd.mrkdwn(f.Value)))
	}
	if b := rd.blocks(a.Blocks); b != "" {
		lines = append(lines, b)
	}
	if len(lines) == 0 && a.Fallback != "" {
		lines = append(lines, rd.mrkdwn(a.Fallback))
	}
	if len(lines) == 0 {
		return ""
	}
	body := strings.Join(lines, "\n")
	if a.IsMsgUnfurl {
		var q []string
		for _, l := range strings.Split(body, "\n") {
			q = append(q, "> "+l)
		}
		return "Shared message:\n" + strings.Join(q, "\n")
	}
	return "Attachment:\n" + body
}

func joinNonEmpty(parts []string, sep string) string {
	var keep []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, sep)
}

func normalizeSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// Excerpt shortens text to roughly n characters on a word boundary, single line.
func Excerpt(s string, n int) string {
	s = normalizeSpace(s)
	if len(s) <= n {
		return s
	}
	cut := strings.LastIndex(s[:n], " ")
	if cut < n/2 {
		cut = n
	}
	return s[:cut] + "…"
}
