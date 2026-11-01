package session

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/daffirds/agent-shift/internal/config"
	"github.com/daffirds/agent-shift/internal/slack"
)

// instructions is appended to the engine's own system prompt.
func instructions(a *config.Agent, ws *Workspace, r *Record) string {
	var b strings.Builder
	fmt.Fprintf(&b, `# Slack session (agent-shift)

You are the %q agent, running unattended on %s's computer. You work through Slack on behalf of %s (Slack user %s) in the %q workspace. A Slack message started this session.

How this session works:
- Nobody reads your plain text output. People only see what you send with the Slack tools of the "agentshift" MCP server: slack_reply, slack_send_message, slack_read_thread, slack_read_channel, slack_search, slack_get_user, slack_find_conversations, slack_download_file, slack_upload_file, slack_react, slack_edit_message, slack_delete_message. Messages are sent from %s's own Slack account and the footer "%s" is added automatically — never add it yourself.
- Your home thread is the thread of the triggering message in %s (channel %s, thread_ts %s). slack_reply posts there; prefer it for anything about this task.
- New replies and reactions in that thread are delivered to you as new messages, even while you are working. Treat them as updates or instructions from the people involved.
- Keep Slack messages short and useful: briefly acknowledge long tasks, ask when you are blocked, and report results with links. Don't narrate every step.
- agent-shift shows no progress of its own, so nobody can tell you are working unless you show it. Reactions are the quiet way: slack_react on the triggering message, e.g. :eyes: when you start something that takes a while, then :white_check_mark: when it's done (remove: true takes :eyes: off).
- Slack formatting: write standard Markdown. Mention people with <@USERID> and channels with <#CHANNELID>.
- Files: slack_download_file saves attachments locally so you can read them; slack_upload_file shares files you produce.
- You are writing as a real person. Only message other people or channels when the task calls for it, and never share secrets.
`, a.Name, ws.UserName, ws.UserName, ws.UserID, ws.Name,
		ws.UserName, a.FooterText(), r.ChannelName, r.Channel, r.ThreadTS)
	if r.Worktree != "" {
		fmt.Fprintf(&b, "- Your working directory %s is a dedicated git worktree (detached HEAD) created for this session. Create a branch before committing.\n", r.Workdir)
	}
	if p := strings.TrimSpace(a.SystemPrompt); p != "" {
		b.WriteString("\n# Your instructions\n\n")
		b.WriteString(p)
		b.WriteString("\n")
	}
	return b.String()
}

func fmtTS(ts string) string {
	t := slack.TSTime(ts)
	if t.IsZero() {
		return ""
	}
	return t.Format("Mon 2006-01-02 15:04 MST")
}

// lateAfter is how old a message must be to count as picked up late (missed while offline).
const lateAfter = 2 * time.Minute

// lateBy is how long ago the message was posted, or 0 if it is being handled about live.
func lateBy(ts string) time.Duration {
	t := slack.TSTime(ts)
	if d := time.Since(t); !t.IsZero() && d >= lateAfter {
		return d
	}
	return 0
}

// roughly formats a delay for the agent: "40 minutes", "15 hours", "3 days".
func roughly(d time.Duration) string {
	n, unit := int(d.Minutes()), "minute"
	switch {
	case d >= 48*time.Hour:
		n, unit = int(d.Hours()/24), "day"
	case d >= time.Hour:
		n, unit = int(d.Round(time.Hour).Hours()), "hour"
	}
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// triggerPrompt is the first user message of a session. earlier holds replies the thread
// already had when the message was picked up late (after downtime).
func triggerPrompt(ctx context.Context, ws *Workspace, r *Record, m *slack.Message, text string, earlier []slack.Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "A new Slack message matched your trigger.\n\n")
	if late := lateBy(m.TS); late > 0 {
		fmt.Fprintf(&b, "It was posted %s ago while agent-shift (the program relaying Slack to you) was offline — this computer was off, asleep or disconnected — so you are picking it up late. Make sure it still needs doing before you act.\n\n", roughly(late))
	}
	fmt.Fprintf(&b, "From: %s (%s)\n", ws.Dir.SenderName(ctx, m), m.SenderID())
	fmt.Fprintf(&b, "Conversation: %s (%s)\n", r.ChannelName, r.Channel)
	fmt.Fprintf(&b, "Message ts (your thread_ts): %s\n", m.TS)
	if t := fmtTS(m.TS); t != "" {
		fmt.Fprintf(&b, "Time: %s\n", t)
	}
	if r.Permalink != "" {
		fmt.Fprintf(&b, "Link: %s\n", r.Permalink)
	}
	fmt.Fprintf(&b, "\n<slack_message>\n%s\n</slack_message>\n", text)
	if len(earlier) > 0 {
		b.WriteString("\nReplies already posted in the thread (oldest first):\n\n")
		for i := range earlier {
			e := &earlier[i]
			fmt.Fprintf(&b, "--- %s (%s) at %s, ts %s\n%s\n", ws.Dir.SenderName(ctx, e), e.SenderID(), fmtTS(e.TS), e.TS, slack.Render(ctx, ws.Dir, e))
		}
		b.WriteString("--- end of replies ---\n")
	}
	return b.String()
}

// replyPrompt delivers a new message from the session's thread. late marks a reply that was
// posted while agent-shift was offline.
func replyPrompt(ctx context.Context, ws *Workspace, m *slack.Message, late bool) string {
	who := ws.Dir.SenderName(ctx, m)
	if m.User == ws.UserID {
		who += " — the person you work for"
	}
	var notes []string
	if m.Subtype == "thread_broadcast" {
		notes = append(notes, "also sent to the channel")
	}
	if late {
		notes = append(notes, "posted while agent-shift was offline")
	}
	extra := ""
	if len(notes) > 0 {
		extra = " (" + strings.Join(notes, "; ") + ")"
	}
	return fmt.Sprintf("New reply in your Slack thread from %s (%s) at %s, ts %s%s:\n\n<slack_message>\n%s\n</slack_message>\n",
		who, m.SenderID(), fmtTS(m.TS), m.TS, extra, slack.Render(ctx, ws.Dir, m))
}

// reactionPrompt delivers a reaction on a message of the session's thread.
func reactionPrompt(ctx context.Context, ws *Workspace, r *slack.ReactionEvent, target *slack.Message, ownTarget bool) string {
	who := ws.Dir.UserName(ctx, r.User)
	if r.User == ws.UserID {
		who += " — the person you work for"
	}
	what := "the message"
	if ownTarget {
		what = "your message"
	}
	excerpt := ""
	if target != nil {
		excerpt = fmt.Sprintf(": %q", slack.Excerpt(slack.Render(ctx, ws.Dir, target), 280))
	}
	verb := "reacted with"
	if !r.Added {
		verb = "removed their reaction"
	}
	return fmt.Sprintf("%s (%s) %s :%s: on %s (ts %s)%s\n", who, r.User, verb, r.Name, what, r.TS, excerpt)
}

// resumeContext is prepended when a session has to be rebuilt from Slack history
// (e.g. the CLI's own conversation could not be resumed).
func resumeContext(ctx context.Context, ws *Workspace, r *Record) string {
	page, err := ws.API.Replies(ctx, r.Channel, r.ThreadTS, slack.HistoryOptions{Limit: 100})
	if err != nil || len(page.Messages) == 0 {
		return "Note: your previous session could not be restored. Read the thread with slack_read_thread before continuing.\n\n"
	}
	var b strings.Builder
	b.WriteString("Note: your previous session could not be restored. Here is the Slack thread so far (oldest first):\n\n")
	for i := range page.Messages {
		m := &page.Messages[i]
		tag := ""
		if m.HasBlockID(slack.FooterBlockID) {
			tag = " [sent by you]"
		}
		fmt.Fprintf(&b, "--- %s (%s) ts %s%s\n%s\n", ws.Dir.SenderName(ctx, m), m.SenderID(), m.TS, tag, slack.Render(ctx, ws.Dir, m))
	}
	b.WriteString("--- end of thread ---\n\n")
	return b.String()
}
