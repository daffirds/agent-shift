package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/daffirds/agent-shift/internal/app"
	"github.com/daffirds/agent-shift/internal/slack"
)

// Binding ties a tool server to one session: its thread, identity and bookkeeping.
type Binding struct {
	Dir      *slack.Directory
	Self     string // the connected user's ID (messages are sent as this user)
	SelfName string
	Channel  string // the session's conversation
	ThreadTS string // the session's thread (the trigger message)
	Workdir  string // relative upload paths resolve here
	FilesDir string // downloads are saved here
	Footer   string
	Log      *slog.Logger
	Echo     Echo
}

// Echo records what a session posts, so the relay can tell agent output from human input
// (both come from the same Slack account) and so edits are limited to the agent's own messages.
type Echo interface {
	PostedMessage(channel, threadTS, ts string)
	UploadedFile(fileID string)
	Reacted(channel, ts, emoji string)
	Owns(channel, ts string) bool
}

func newServer(b *Binding) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: app.Name, Title: "Slack (via " + app.Name + ")", Version: app.Version},
		&mcp.ServerOptions{Instructions: "Slack tools acting as " + b.SelfName + ". Messages are sent from their real account with a footer appended automatically."})

	mcp.AddTool(s, &mcp.Tool{Name: "slack_reply", Description: "Reply in the Slack thread this session belongs to. " +
		"Write standard Markdown. Mention people with <@USERID>. The footer is added automatically."}, b.reply)
	mcp.AddTool(s, &mcp.Tool{Name: "slack_send_message", Description: "Send a message to any conversation: a channel (#name or C…/G… ID), " +
		"a person (@name, email or U… ID — opens a DM), or a group DM (comma-separated people). Optionally reply in a thread with thread_ts. " +
		"Only message people/channels when the task requires it: you are writing as a real person."}, b.send)
	mcp.AddTool(s, &mcp.Tool{Name: "slack_read_thread", Description: "Read all messages of a thread. Defaults to this session's thread.",
		Annotations: readOnly()}, b.readThread)
	mcp.AddTool(s, &mcp.Tool{Name: "slack_read_channel", Description: "Read recent top-level messages of a channel, DM or group DM (oldest first).",
		Annotations: readOnly()}, b.readChannel)
	mcp.AddTool(s, &mcp.Tool{Name: "slack_search", Description: "Search Slack messages with Slack search syntax, e.g. `deploy in:#ops from:@ana after:2025-01-01`.",
		Annotations: readOnly()}, b.search)
	mcp.AddTool(s, &mcp.Tool{Name: "slack_get_user", Description: "Look up a person by ID, @handle, name or email.",
		Annotations: readOnly()}, b.getUser)
	mcp.AddTool(s, &mcp.Tool{Name: "slack_find_conversations", Description: "Find channels, DMs and group DMs you are a member of by name.",
		Annotations: readOnly()}, b.findConversations)
	mcp.AddTool(s, &mcp.Tool{Name: "slack_download_file", Description: "Download a Slack file (by file ID, e.g. F0123ABC) to a local path you can read.",
		Annotations: readOnly()}, b.download)
	mcp.AddTool(s, &mcp.Tool{Name: "slack_upload_file", Description: "Upload a local file to Slack. Defaults to this session's thread."}, b.upload)
	mcp.AddTool(s, &mcp.Tool{Name: "slack_react", Description: "Add (or remove) an emoji reaction. Defaults to the message that started this session."}, b.react)
	mcp.AddTool(s, &mcp.Tool{Name: "slack_edit_message", Description: "Edit a message previously sent by this session."}, b.edit)
	mcp.AddTool(s, &mcp.Tool{Name: "slack_delete_message", Description: "Delete a message previously sent by this session.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true)}}, b.deleteMessage)
	return s
}

func readOnly() *mcp.ToolAnnotations { return &mcp.ToolAnnotations{ReadOnlyHint: true} }

func ptr[T any](v T) *T { return &v }

func text(format string, args ...any) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}}}, nil, nil
}

func (b *Binding) api() *slack.Client { return b.Dir.API() }

// post sends text (split into several messages if very long), returning the last message.
func (b *Binding) post(ctx context.Context, channel, threadTS, body string, broadcast bool) (*slack.PostResult, error) {
	if strings.TrimSpace(body) == "" {
		return nil, errors.New("text is empty")
	}
	var last *slack.PostResult
	for i, chunk := range slack.Split(body, slack.MaxMarkdown) {
		// Every chunk carries the footer block: it is how the relay recognises its own posts.
		fallback, blocks := slack.Compose(chunk, b.Footer)
		res, err := b.api().PostMessage(ctx, slack.PostParams{
			Channel: channel, ThreadTS: threadTS, Text: fallback, Blocks: blocks,
			ReplyBroadcast: broadcast && i == 0 && threadTS != "",
		})
		if err != nil {
			return nil, err
		}
		b.Echo.PostedMessage(res.Channel, threadTS, res.TS)
		last = res
	}
	return last, nil
}

type replyIn struct {
	Text      string `json:"text" jsonschema:"the message, in Markdown"`
	Broadcast bool   `json:"broadcast,omitempty" jsonschema:"also show the reply in the channel"`
}

func (b *Binding) reply(ctx context.Context, _ *mcp.CallToolRequest, in replyIn) (*mcp.CallToolResult, any, error) {
	res, err := b.post(ctx, b.Channel, b.ThreadTS, in.Text, in.Broadcast)
	if err != nil {
		return nil, nil, err
	}
	link, _ := b.api().Permalink(ctx, res.Channel, res.TS)
	return text("Sent (ts=%s) %s", res.TS, link)
}

type sendIn struct {
	To       string `json:"to" jsonschema:"#channel, channel ID, @person, email, user ID, or comma-separated people for a group DM"`
	Text     string `json:"text" jsonschema:"the message, in Markdown"`
	ThreadTS string `json:"thread_ts,omitempty" jsonschema:"reply inside this thread (ts of its parent message)"`
}

func (b *Binding) send(ctx context.Context, _ *mcp.CallToolRequest, in sendIn) (*mcp.CallToolResult, any, error) {
	ch, err := b.Dir.ResolveConversation(ctx, in.To)
	if err != nil {
		return nil, nil, err
	}
	res, err := b.post(ctx, ch, in.ThreadTS, in.Text, false)
	if err != nil {
		return nil, nil, err
	}
	link, _ := b.api().Permalink(ctx, res.Channel, res.TS)
	return text("Sent to %s (channel=%s ts=%s) %s", b.Dir.ConversationName(ctx, ch), res.Channel, res.TS, link)
}

type readThreadIn struct {
	Channel  string `json:"channel,omitempty" jsonschema:"conversation of the thread (default: this session's)"`
	ThreadTS string `json:"thread_ts,omitempty" jsonschema:"ts of the thread's parent message (default: this session's thread)"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max messages (default 100)"`
}

func (b *Binding) readThread(ctx context.Context, _ *mcp.CallToolRequest, in readThreadIn) (*mcp.CallToolResult, any, error) {
	ch, ts := b.Channel, b.ThreadTS
	if in.Channel != "" {
		var err error
		if ch, err = b.Dir.ResolveConversation(ctx, in.Channel); err != nil {
			return nil, nil, err
		}
	}
	if in.ThreadTS != "" {
		ts = in.ThreadTS
	}
	limit := clamp(in.Limit, 100, 1000)
	var msgs []slack.Message
	cursor := ""
	for len(msgs) < limit {
		page, err := b.api().Replies(ctx, ch, ts, slack.HistoryOptions{Limit: min(200, limit-len(msgs)), Cursor: cursor})
		if err != nil {
			return nil, nil, err
		}
		msgs = append(msgs, page.Messages...)
		if cursor = page.NextCursor(); cursor == "" || !page.HasMore {
			break
		}
	}
	return text("%s", b.renderMessages(ctx, msgs))
}

type readChannelIn struct {
	Channel string `json:"channel" jsonschema:"#channel, channel ID, @person (their DM) or user ID"`
	Limit   int    `json:"limit,omitempty" jsonschema:"max messages (default 20, max 200)"`
	Before  string `json:"before,omitempty" jsonschema:"only messages before this ts"`
	After   string `json:"after,omitempty" jsonschema:"only messages after this ts"`
}

func (b *Binding) readChannel(ctx context.Context, _ *mcp.CallToolRequest, in readChannelIn) (*mcp.CallToolResult, any, error) {
	ch, err := b.Dir.ResolveConversation(ctx, in.Channel)
	if err != nil {
		return nil, nil, err
	}
	page, err := b.api().History(ctx, ch, slack.HistoryOptions{Limit: clamp(in.Limit, 20, 200), Latest: in.Before, Oldest: in.After})
	if err != nil {
		return nil, nil, err
	}
	msgs := page.Messages
	slices.Reverse(msgs)
	header := fmt.Sprintf("%s (%s), %d messages, oldest first:\n\n", b.Dir.ConversationName(ctx, ch), ch, len(msgs))
	return text("%s%s", header, b.renderMessages(ctx, msgs))
}

func (b *Binding) renderMessages(ctx context.Context, msgs []slack.Message) string {
	if len(msgs) == 0 {
		return "(no messages)"
	}
	var sb strings.Builder
	for i := range msgs {
		m := &msgs[i]
		fmt.Fprintf(&sb, "[ts=%s %s] %s (%s)", m.TS, tsTime(m.TS), b.Dir.SenderName(ctx, m), m.SenderID())
		if m.ReplyCount > 0 {
			fmt.Fprintf(&sb, " [thread with %d replies]", m.ReplyCount)
		}
		if m.HasBlockID(slack.FooterBlockID) {
			sb.WriteString(" [sent via agent-shift]")
		}
		if len(m.Reactions) > 0 {
			var rs []string
			for _, r := range m.Reactions {
				rs = append(rs, fmt.Sprintf(":%s:×%d", r.Name, r.Count))
			}
			sb.WriteString(" reactions: " + strings.Join(rs, " "))
		}
		sb.WriteString("\n")
		sb.WriteString(indent(slack.Render(ctx, b.Dir, m)))
		sb.WriteString("\n\n")
	}
	return strings.TrimSpace(sb.String())
}

type searchIn struct {
	Query string `json:"query" jsonschema:"Slack search query"`
	Count int    `json:"count,omitempty" jsonschema:"max results (default 20, max 100)"`
}

func (b *Binding) search(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, any, error) {
	matches, total, err := b.api().SearchMessages(ctx, in.Query, clamp(in.Count, 20, 100))
	if err != nil {
		return nil, nil, err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d total matches, showing %d:\n\n", total, len(matches))
	for _, m := range matches {
		who := m.Username
		if m.User != "" {
			who = b.Dir.UserName(ctx, m.User) + " (" + m.User + ")"
		}
		fmt.Fprintf(&sb, "[ts=%s %s] #%s (%s) %s:\n%s\n%s\n\n", m.TS, tsTime(m.TS), m.Channel.Name, m.Channel.ID, who,
			indent(slack.Render(ctx, b.Dir, &slack.Message{Text: m.Text})), m.Permalink)
	}
	return text("%s", strings.TrimSpace(sb.String()))
}

type userIn struct {
	Query string `json:"query" jsonschema:"user ID, @handle, name or email"`
}

func (b *Binding) getUser(ctx context.Context, _ *mcp.CallToolRequest, in userIn) (*mcp.CallToolResult, any, error) {
	u, err := b.Dir.FindUser(ctx, in.Query)
	if err != nil {
		return nil, nil, err
	}
	return text("id: %s\nhandle: @%s\ndisplay name: %s\nreal name: %s\ntitle: %s\nemail: %s\ntimezone: %s\nbot: %v\nmention syntax: <@%s>",
		u.ID, u.Name, u.Profile.DisplayName, u.Profile.RealName, u.Profile.Title, u.Profile.Email, u.TZ, u.IsBot, u.ID)
}

type findIn struct {
	Query string `json:"query" jsonschema:"part of the channel name or person's name"`
}

func (b *Binding) findConversations(ctx context.Context, _ *mcp.CallToolRequest, in findIn) (*mcp.CallToolResult, any, error) {
	convs, err := b.Dir.MyConversations(ctx)
	if err != nil {
		return nil, nil, err
	}
	q := strings.ToLower(strings.TrimPrefix(in.Query, "#"))
	var out []string
	for i := range convs {
		c := &convs[i]
		label := c.Name
		if c.IsIM {
			label = b.Dir.UserName(ctx, c.User)
		}
		if q == "" || strings.Contains(strings.ToLower(label), q) || strings.Contains(strings.ToLower(c.Name), q) {
			out = append(out, fmt.Sprintf("%s — %s (%s)", c.ID, b.Dir.Label(ctx, c), c.Kind()))
		}
		if len(out) >= 30 {
			break
		}
	}
	if len(out) == 0 {
		return text("No conversations match %q.", in.Query)
	}
	return text("%s", strings.Join(out, "\n"))
}

type downloadIn struct {
	FileID string `json:"file_id" jsonschema:"Slack file ID, e.g. F0123ABCDEF"`
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func (b *Binding) download(ctx context.Context, _ *mcp.CallToolRequest, in downloadIn) (*mcp.CallToolResult, any, error) {
	f, err := b.api().FileInfo(ctx, in.FileID)
	if err != nil {
		return nil, nil, err
	}
	src := f.URLPrivateDownload
	if src == "" {
		src = f.URLPrivate
	}
	if src == "" {
		return nil, nil, fmt.Errorf("file %s has no downloadable content (mode %s)", f.ID, f.Mode)
	}
	if err := os.MkdirAll(b.FilesDir, 0o700); err != nil {
		return nil, nil, err
	}
	name := unsafeName.ReplaceAllString(f.Name, "_")
	if name == "" {
		name = "file"
	}
	dst := filepath.Join(b.FilesDir, f.ID+"_"+name)
	out, err := os.Create(dst)
	if err != nil {
		return nil, nil, err
	}
	n, err := b.api().Download(ctx, src, out, 1<<30)
	out.Close()
	if err != nil {
		os.Remove(dst)
		return nil, nil, err
	}
	return text("Saved %s (%s, %s) to %s", f.Name, f.Mimetype, slack.HumanSize(n), dst)
}

type uploadIn struct {
	Path     string `json:"path" jsonschema:"local file path (relative paths are relative to the working directory)"`
	To       string `json:"to,omitempty" jsonschema:"conversation to share into (default: this session's thread)"`
	ThreadTS string `json:"thread_ts,omitempty" jsonschema:"thread to share into when 'to' is set"`
	Title    string `json:"title,omitempty"`
	Comment  string `json:"comment,omitempty" jsonschema:"message posted with the file"`
}

func (b *Binding) upload(ctx context.Context, _ *mcp.CallToolRequest, in uploadIn) (*mcp.CallToolResult, any, error) {
	path := in.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(b.Workdir, path)
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if st.IsDir() {
		return nil, nil, fmt.Errorf("%s is a directory", path)
	}
	ch, thread := b.Channel, b.ThreadTS
	if in.To != "" {
		if ch, err = b.Dir.ResolveConversation(ctx, in.To); err != nil {
			return nil, nil, err
		}
		thread = in.ThreadTS
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	comment := in.Comment
	if b.Footer != "" {
		comment = strings.TrimSpace(comment + "\n\n_" + b.Footer + "_")
	}
	file, err := b.api().UploadFile(ctx, slack.UploadParams{
		Channel: ch, ThreadTS: thread, Filename: filepath.Base(path), Title: in.Title,
		InitialComment: comment, Content: f, Size: st.Size(), OnFileID: b.Echo.UploadedFile,
	})
	if err != nil {
		return nil, nil, err
	}
	return text("Uploaded %s as %s %s", filepath.Base(path), file.ID, file.Permalink)
}

type reactIn struct {
	Emoji   string `json:"emoji" jsonschema:"emoji name without colons, e.g. white_check_mark"`
	Channel string `json:"channel,omitempty" jsonschema:"conversation of the message (default: this session's)"`
	TS      string `json:"ts,omitempty" jsonschema:"ts of the message (default: the message that started this session)"`
	Remove  bool   `json:"remove,omitempty" jsonschema:"remove the reaction instead of adding it"`
}

func (b *Binding) react(ctx context.Context, _ *mcp.CallToolRequest, in reactIn) (*mcp.CallToolResult, any, error) {
	ch, ts := b.Channel, b.ThreadTS
	if in.Channel != "" {
		var err error
		if ch, err = b.Dir.ResolveConversation(ctx, in.Channel); err != nil {
			return nil, nil, err
		}
	}
	if in.TS != "" {
		ts = in.TS
	}
	emoji := strings.Trim(in.Emoji, ": ")
	b.Echo.Reacted(ch, ts, emoji)
	var err error
	if in.Remove {
		err = b.api().RemoveReaction(ctx, ch, ts, emoji)
	} else {
		err = b.api().AddReaction(ctx, ch, ts, emoji)
	}
	if err != nil {
		return nil, nil, err
	}
	return text("Done.")
}

type editIn struct {
	Channel string `json:"channel" jsonschema:"conversation ID of the message"`
	TS      string `json:"ts" jsonschema:"ts of a message this session sent"`
	Text    string `json:"text" jsonschema:"the new message, in Markdown"`
}

func (b *Binding) edit(ctx context.Context, _ *mcp.CallToolRequest, in editIn) (*mcp.CallToolResult, any, error) {
	if !b.Echo.Owns(in.Channel, in.TS) {
		return nil, nil, errors.New("you can only edit messages this session sent")
	}
	fallback, blocks := slack.Compose(in.Text, b.Footer)
	if err := b.api().UpdateMessage(ctx, in.Channel, in.TS, fallback, blocks); err != nil {
		return nil, nil, err
	}
	return text("Edited.")
}

type deleteIn struct {
	Channel string `json:"channel" jsonschema:"conversation ID of the message"`
	TS      string `json:"ts" jsonschema:"ts of a message this session sent"`
}

func (b *Binding) deleteMessage(ctx context.Context, _ *mcp.CallToolRequest, in deleteIn) (*mcp.CallToolResult, any, error) {
	if !b.Echo.Owns(in.Channel, in.TS) {
		return nil, nil, errors.New("you can only delete messages this session sent")
	}
	if err := b.api().DeleteMessage(ctx, in.Channel, in.TS); err != nil {
		return nil, nil, err
	}
	return text("Deleted.")
}

func clamp(v, def, max int) int {
	if v <= 0 {
		return def
	}
	return min(v, max)
}

func indent(s string) string {
	if s == "" {
		return "  (empty)"
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

func tsTime(ts string) string {
	sec, _, _ := strings.Cut(ts, ".")
	n, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return ""
	}
	return time.Unix(n, 0).Format("2006-01-02 15:04")
}
