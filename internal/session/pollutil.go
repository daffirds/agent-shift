package session

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/daffirds/agent-shift/internal/slack"
)

// maxCatchUpPages bounds how much one read of a conversation or thread pages through (200 messages a page).
const maxCatchUpPages = 25

// watched lists the conversations the workspace's enabled agents trigger on.
func (m *Manager) watched(teamID string) []string {
	var out []string
	for _, a := range m.cfg.Load().Agents {
		if a.Disabled || a.Trigger.Workspace != teamID {
			continue
		}
		for _, c := range a.Trigger.Conversations {
			if c.ID != "" && !slices.Contains(out, c.ID) {
				out = append(out, c.ID)
			}
		}
	}
	return out
}

// handledMessages returns the trigger messages and own posts of every persisted session,
// ended ones included, so a replay never starts a second session for a message.
func handledMessages(teamID string) map[string]bool {
	recs, _ := LoadRecords()
	out := map[string]bool{}
	for _, r := range recs {
		if r.TeamID != teamID {
			continue
		}
		out[threadKey(teamID, r.Channel, r.ThreadTS)] = true
		for _, p := range r.Posted {
			if ch, ts, ok := strings.Cut(p, "/"); ok {
				out[threadKey(teamID, ch, ts)] = true
			}
		}
	}
	return out
}

// fetchAll pages through conversations.history or .replies and returns the messages oldest first.
func fetchAll(fetch func(cursor string) (*slack.MessagesPage, error)) ([]slack.Message, error) {
	var out []slack.Message
	cursor := ""
	for range maxCatchUpPages {
		page, err := fetch(cursor)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Messages...)
		if cursor = page.NextCursor(); !page.HasMore || cursor == "" {
			break
		}
	}
	slices.SortStableFunc(out, func(a, b slack.Message) int { return slack.TSTime(a.TS).Compare(slack.TSTime(b.TS)) })
	return out, nil
}

// transient reports whether a failed Slack call is worth retrying later.
func transient(err error) bool {
	var ae *slack.APIError
	if errors.As(err, &ae) {
		return slices.Contains([]string{"ratelimited", "internal_error", "fatal_error", "service_unavailable", "request_timeout"}, ae.Code)
	}
	return !errors.Is(err, context.Canceled)
}
