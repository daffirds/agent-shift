package session

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/daffirds/agent-shift/internal/config"
	"github.com/daffirds/agent-shift/internal/slack"
)

// CatchUpResult summarises a catch-up pass.
type CatchUpResult struct {
	Conversations int `json:"conversations"` // trigger conversations scanned
	Threads       int `json:"threads"`       // session threads checked for replies
	Sessions      int `json:"sessions"`      // sessions started
	Replies       int `json:"replies"`       // replies steered into existing sessions
}

// maxCatchUpPages bounds how much one conversation or thread can replay (200 messages a page).
const maxCatchUpPages = 25

// CatchUp replays Slack activity the daemon missed while it was offline (computer off or
// asleep, network down): new replies in tracked threads are steered into their sessions, then
// trigger messages posted after since start sessions, oldest first. activeSince maps agent
// names to when they were enabled; an agent only picks up messages posted after that, i.e.
// what it would have seen live. Messages that were already handled are skipped, so replaying
// an overlapping window is harmless. Like HandleEvent, call it from the workspace's event
// worker so it stays ordered with live events.
//
// The error reports transient failures only (network, Slack outages): retry the same window
// later. Permanent ones, such as a channel you have left, are logged and skipped.
func (m *Manager) CatchUp(ctx context.Context, teamID string, since time.Time, activeSince map[string]time.Time) (CatchUpResult, error) {
	var res CatchUpResult
	m.mu.Lock()
	ws := m.ws[teamID]
	var tracked []*Session
	for _, s := range m.sessions {
		if s.rec.TeamID == teamID {
			tracked = append(tracked, s)
		}
	}
	m.mu.Unlock()
	if ws == nil {
		return res, nil
	}
	oldest := slack.FormatTS(since)
	var errs []error
	failed := func(what string, err error) {
		if transient(err) {
			errs = append(errs, err)
		} else {
			m.log.Warn("catch-up: skipping "+what, "err", err)
		}
	}

	slices.SortFunc(tracked, func(a, b *Session) int { return strings.Compare(a.rec.ThreadTS, b.rec.ThreadTS) })
	for _, s := range tracked {
		r := s.snapshot()
		res.Threads++
		msgs, err := fetchAll(func(cursor string) (*slack.MessagesPage, error) {
			return ws.API.Replies(ctx, r.Channel, r.ThreadTS, slack.HistoryOptions{Oldest: oldest, Limit: 200, Cursor: cursor})
		})
		if err != nil {
			failed("thread of session "+r.ID, err)
			continue
		}
		for i := range msgs {
			msg := &msgs[i]
			if msg.TS == r.ThreadTS {
				continue // the parent comes with every page
			}
			msg.Channel = r.Channel
			if m.handleMessage(ctx, ws, msg, nil) == steered {
				res.Replies++
			}
		}
	}

	handled := handledMessages(teamID)
	for _, conv := range m.watched(teamID) {
		res.Conversations++
		msgs, err := fetchAll(func(cursor string) (*slack.MessagesPage, error) {
			return ws.API.History(ctx, conv, slack.HistoryOptions{Oldest: oldest, Limit: 200, Cursor: cursor})
		})
		if err != nil {
			failed("conversation "+ws.Dir.ConversationName(ctx, conv), err)
			continue
		}
		for i := range msgs {
			msg := &msgs[i]
			msg.Channel = conv
			if !msg.IsThreadReply() && handled[threadKey(teamID, conv, msg.TS)] {
				continue
			}
			posted := slack.TSTime(msg.TS)
			eligible := func(a *config.Agent) bool {
				t, ok := activeSince[a.Name]
				return ok && !t.After(posted)
			}
			switch m.handleMessage(ctx, ws, msg, eligible) {
			case started:
				res.Sessions++
			case steered:
				res.Replies++ // a reply also sent to the channel
			}
		}
	}
	return res, errors.Join(errs...)
}

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
