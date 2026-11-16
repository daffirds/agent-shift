package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/daffirds/agent-shift/internal/config"
	"github.com/daffirds/agent-shift/internal/slack"
)

// Polling replaces Slack's push delivery. Slack never sends agent-shift anything, so there is
// nothing for Slack to fail to deliver, or to switch off, while the computer is off. Each
// workspace gets one Poller, ticked by one worker so messages stay in order:
//
//   - hot threads (a session working, or recent activity) are read every few seconds;
//   - the agents' trigger conversations are read on a budget that fits Slack's rate limits;
//   - every other tracked thread, however many there are, is covered by one search.messages
//     sweep: a new reply anywhere shows up there, and its thread turns hot;
//   - spare budget re-reads cold threads round-robin, which also catches anything search's
//     index missed (and covers threads entirely when the token lacks search:read).
//
// Every cursor starts at `since` (the daemon's last watermark), so the first passes after a
// restart or a wake from sleep replay what was missed; handling is idempotent.

const (
	pollTick = time.Second

	hotFor      = 15 * time.Minute // a thread stays hot this long after its last activity
	hotEvery    = 4 * time.Second  // fastest re-read of a hot thread
	convEvery   = 5 * time.Second  // fastest re-read of a trigger conversation
	searchEvery = 20 * time.Second
	coldEvery   = 6 * time.Second // at most one cold-thread read this often

	// searchLag: search's index trails a few seconds behind; a sweep only vouches for
	// messages older than this.
	searchLag      = 2 * time.Minute
	maxSearchPages = 10
	failBackoff    = time.Minute

	// Shares of Slack's per-method limits (Tier 3 ≈ 50/min, search Tier 2 ≈ 20/min), leaving
	// room for the agents' own tool calls.
	historyPerMin = 30
	repliesPerMin = 36
	searchPerMin  = 6
)

// PollStatus describes a workspace's polling, for `agent-shift status` and the UI.
type PollStatus struct {
	LastPoll      time.Time `json:"last_poll,omitzero"`
	LastOK        time.Time `json:"last_ok,omitzero"`
	Covered       time.Time `json:"covered,omitzero"` // everything posted before this has been seen
	Conversations int       `json:"conversations"`    // trigger conversations polled
	Threads       int       `json:"threads"`          // session threads tracked
	Hot           int       `json:"hot"`              // of which read every few seconds
	Search        string    `json:"search"`           // "on", "idle" (no cold threads) or why it is off
	Sessions      int       `json:"sessions"`         // sessions started by this poller
	Replies       int       `json:"replies"`          // replies delivered by this poller
	Error         string    `json:"error,omitempty"`
}

// Poller polls one workspace. Tick must be called from a single goroutine; Status and
// Covered are safe from any.
type Poller struct {
	m    *Manager
	team string
	log  *slog.Logger
	// ActiveSince maps agent names to when they were enabled: an agent only starts sessions
	// for messages posted after that, i.e. what it would have seen live.
	ActiveSince func() map[string]time.Time

	since   time.Time       // start of the window this poller answers for
	handled map[string]bool // trigger messages of past sessions, so a replay never restarts one

	convs     map[string]*convCursor
	threads   map[string]*threadCursor // channel/thread_ts
	searchAt  time.Time                // next sweep
	searchCov time.Time                // the sweeps have seen every reply posted before this
	searchOff string                   // why search is unusable ("" = usable)
	coldAt    time.Time
	lastTick  time.Time
	history   bucket
	replies   bucket
	searchB   bucket

	mu     sync.Mutex
	status PollStatus
}

type convCursor struct {
	latest  string    // newest ts seen
	covered time.Time // read up to here
	next    time.Time
}

type threadCursor struct {
	channel, ts string
	latest      string // newest reply ts seen
	covered     time.Time
	next        time.Time
	hotUntil    time.Time
	reacts      map[string]map[string]bool // message ts → "emoji user" pairs
	baseline    bool                       // reacts holds a first reading
	gone        bool                       // Slack says the thread no longer exists
}

// NewPoller starts polling a workspace from since.
func (m *Manager) NewPoller(teamID string, since time.Time) *Poller {
	now := time.Now()
	p := &Poller{
		m: m, team: teamID, log: m.log.With("workspace", teamID, "component", "poll"),
		since: since, handled: handledMessages(teamID),
		convs: map[string]*convCursor{}, threads: map[string]*threadCursor{},
		searchCov: since, lastTick: now,
		history: newBucket(historyPerMin, now), replies: newBucket(repliesPerMin, now), searchB: newBucket(searchPerMin, now),
	}
	p.status.Covered = since
	return p
}

// Status returns the latest polling status.
func (p *Poller) Status() PollStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

// Covered is the watermark: everything posted before it has been handled.
func (p *Poller) Covered() time.Time { return p.Status().Covered }

// Tick does the polling that is due. It returns how long until it wants to run again.
func (p *Poller) Tick(ctx context.Context) time.Duration {
	p.m.mu.Lock()
	ws := p.m.ws[p.team]
	p.m.mu.Unlock()
	if ws == nil {
		return pollTick
	}
	now := time.Now()
	if gap := now.Sub(p.lastTick); gap > 2*time.Minute {
		p.log.Info("resumed after a pause; catching up on Slack", "paused", gap.Round(time.Second))
	}
	p.lastTick = now
	floor := p.floor(now)
	p.syncTargets(now)

	var errs []error
	var okCalls int
	note := func(what string, err error) {
		if err == nil {
			okCalls++
			return
		}
		if !errors.Is(err, context.Canceled) {
			errs = append(errs, fmt.Errorf("%s: %w", what, err))
		}
	}

	hot := p.hotThreads(now)
	if n := len(hot); n > 0 {
		every := max(hotEvery, time.Duration(n)*time.Minute/repliesPerMin)
		for _, t := range hot {
			if ctx.Err() != nil || now.Before(t.next) || !p.replies.take(now) {
				continue
			}
			note("thread", p.pollThread(ctx, ws, t, floor))
			t.next = time.Now().Add(every)
		}
	}

	if n := len(p.convs); n > 0 {
		every := max(convEvery, time.Duration(n)*time.Minute/historyPerMin)
		for _, id := range sortedKeys(p.convs) {
			c := p.convs[id]
			if ctx.Err() != nil || now.Before(c.next) || !p.history.take(now) {
				continue
			}
			err := p.pollConversation(ctx, ws, id, c, floor)
			note("conversation "+ws.Dir.ConversationName(ctx, id), err)
			c.next = time.Now().Add(every)
			if err != nil && !transient(err) {
				c.next = time.Now().Add(failBackoff)
				c.covered = now // a permanent error (e.g. you left the channel) must not freeze the watermark
			}
		}
	}

	cold := len(p.threads) - len(hot)
	if cold > 0 && p.searchOff == "" && !now.Before(p.searchAt) && ctx.Err() == nil && p.searchB.take(now) {
		note("search", p.sweep(ctx, ws, now, floor))
		p.searchAt = time.Now().Add(searchEvery)
	}

	if cold > 0 && !now.Before(p.coldAt) && p.replies.level(now) >= repliesPerMin/12 && ctx.Err() == nil {
		if t := p.stalest(now); t != nil && p.replies.take(now) {
			note("thread", p.pollThread(ctx, ws, t, floor))
		}
		p.coldAt = time.Now().Add(coldEvery)
	}

	p.mu.Lock()
	p.status.LastPoll = now
	if okCalls > 0 || len(errs) == 0 {
		p.status.LastOK = now
	}
	p.status.Error = ""
	if len(errs) > 0 {
		p.status.Error = errors.Join(errs...).Error()
	}
	p.status.Conversations, p.status.Threads, p.status.Hot = len(p.convs), len(p.threads), len(hot)
	switch {
	case p.searchOff != "":
		p.status.Search = p.searchOff
	case cold == 0:
		p.status.Search = "idle"
	default:
		p.status.Search = "on"
	}
	p.status.Covered = p.covered(now)
	p.mu.Unlock()
	for _, err := range errs {
		p.log.Debug("poll failed", "err", err)
	}
	return pollTick
}

// floor is the oldest time worth reading: daemon.catch_up bounds the replay after a long
// absence (at least a few minutes are always covered, so a short hiccup loses nothing).
func (p *Poller) floor(now time.Time) time.Time {
	window := max(p.m.cfg.Load().Daemon.CatchUpWindow(), 5*time.Minute)
	f := now.Add(-window)
	if p.since.Before(f.Add(-time.Minute)) {
		p.log.Warn("offline for longer than daemon.catch_up; older messages are not replayed",
			"offline_since", p.since.Format(time.DateTime), "catch_up", config.FormatDuration(window))
	}
	p.since = maxTime(p.since, f)
	return f
}

// syncTargets follows config and session changes: which conversations and threads to read.
func (p *Poller) syncTargets(now time.Time) {
	want := map[string]bool{}
	for _, id := range p.m.watched(p.team) {
		want[id] = true
		if p.convs[id] == nil {
			p.convs[id] = &convCursor{latest: slack.FormatTS(p.since), covered: p.since}
		}
	}
	for id := range p.convs {
		if !want[id] {
			delete(p.convs, id)
		}
	}

	p.m.mu.Lock()
	live := map[string]Record{}
	for _, s := range p.m.sessions {
		if s.rec.TeamID == p.team {
			r := s.snapshot()
			live[r.Channel+"/"+r.ThreadTS] = r
		}
	}
	p.m.mu.Unlock()
	for key, r := range live {
		t := p.threads[key]
		if t == nil {
			start := maxTime(p.since, slack.TSTime(r.ThreadTS))
			t = &threadCursor{channel: r.Channel, ts: r.ThreadTS, latest: slack.FormatTS(start), covered: start}
			p.threads[key] = t
		}
		if r.State == StateRunning || r.State == StateQueued || now.Sub(r.LastActive) < hotFor {
			t.hotUntil = maxTime(t.hotUntil, now.Add(time.Second)) // working or recently active
		}
	}
	for key := range p.threads {
		if _, ok := live[key]; !ok {
			delete(p.threads, key)
		}
	}
}

func (p *Poller) hotThreads(now time.Time) []*threadCursor {
	var out []*threadCursor
	for _, t := range p.threads {
		if !t.gone && now.Before(t.hotUntil) {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b *threadCursor) int { return a.next.Compare(b.next) })
	return out
}

// stalest is the cold thread read longest ago.
func (p *Poller) stalest(now time.Time) *threadCursor {
	var best *threadCursor
	for _, t := range p.threads {
		if t.gone || now.Before(t.hotUntil) {
			continue
		}
		if best == nil || t.covered.Before(best.covered) {
			best = t
		}
	}
	return best
}

// covered computes the watermark: the oldest point some target has not been read past.
func (p *Poller) covered(now time.Time) time.Time {
	w := now
	for _, c := range p.convs {
		w = minTime(w, c.covered)
	}
	for _, t := range p.threads {
		if t.gone {
			continue
		}
		cov := t.covered
		if p.searchOff == "" {
			cov = maxTime(cov, p.searchCov)
		}
		w = minTime(w, cov)
	}
	return w
}

// pollConversation reads new top-level messages of a trigger conversation.
func (p *Poller) pollConversation(ctx context.Context, ws *Workspace, id string, c *convCursor, floor time.Time) error {
	start := time.Now()
	oldest := maxTS(c.latest, slack.FormatTS(floor))
	msgs, err := fetchAll(func(cursor string) (*slack.MessagesPage, error) {
		return ws.API.History(ctx, id, slack.HistoryOptions{Oldest: oldest, Limit: 200, Cursor: cursor})
	})
	if err != nil {
		return err
	}
	active := p.activeSince()
	for i := range msgs {
		msg := &msgs[i]
		msg.Channel = id
		c.latest = maxTS(c.latest, msg.TS)
		if !msg.IsThreadReply() && p.handled[threadKey(p.team, id, msg.TS)] {
			continue
		}
		posted := slack.TSTime(msg.TS)
		eligible := func(a *config.Agent) bool {
			t, ok := active[a.Name]
			return ok && !t.After(posted)
		}
		p.count(p.m.handleMessage(ctx, ws, msg, eligible))
	}
	c.covered = start
	return nil
}

// pollThread reads a whole thread: replies newer than the cursor are delivered, and reactions
// that appeared since the last reading are passed on.
func (p *Poller) pollThread(ctx context.Context, ws *Workspace, t *threadCursor, floor time.Time) error {
	start := time.Now()
	msgs, err := fetchAll(func(cursor string) (*slack.MessagesPage, error) {
		return ws.API.Replies(ctx, t.channel, t.ts, slack.HistoryOptions{Limit: 200, Cursor: cursor})
	})
	if err != nil {
		if slack.IsCode(err, "thread_not_found", "channel_not_found", "not_in_channel") {
			t.gone = true
			p.log.Info("thread no longer readable; not polling it", "channel", t.channel, "thread", t.ts, "err", err)
		}
		return err
	}
	cutoff := maxTS(t.latest, slack.FormatTS(floor))
	fresh := false
	seenReacts := map[string]map[string]bool{}
	for i := range msgs {
		msg := &msgs[i]
		msg.Channel = t.channel
		if r := reactionSet(msg); len(r) > 0 {
			seenReacts[msg.TS] = r
		}
		if msg.TS == t.ts || !tsAfter(msg.TS, cutoff) {
			continue
		}
		t.latest = maxTS(t.latest, msg.TS)
		fresh = true
		p.count(p.m.handleMessage(ctx, ws, msg, nil))
	}
	if t.baseline {
		for ts, set := range seenReacts {
			for pair := range set {
				if t.reacts[ts][pair] {
					continue
				}
				name, user, _ := strings.Cut(pair, " ")
				p.m.handleReaction(ctx, ws, &slack.ReactionEvent{Added: true, User: user, Name: name, Channel: t.channel, TS: ts, EventTS: slack.FormatTS(start)})
				fresh = true
			}
		}
	}
	t.reacts, t.baseline = seenReacts, true
	if fresh {
		t.hotUntil = time.Now().Add(hotFor)
	}
	t.covered = start
	return nil
}

func reactionSet(m *slack.Message) map[string]bool {
	var out map[string]bool
	for _, r := range m.Reactions {
		for _, u := range r.Users {
			if out == nil {
				out = map[string]bool{}
			}
			out[r.Name+" "+u] = true
		}
	}
	return out
}

// sweep runs one search for everything posted since the last sweep and wakes the tracked
// threads that got a reply.
func (p *Poller) sweep(ctx context.Context, ws *Workspace, now, floor time.Time) error {
	lower := maxTime(p.searchCov, floor)
	// after: is a whole day in the user's timezone, so start a day early; paging stops at lower.
	query := "after:" + lower.AddDate(0, 0, -2).Format("2006-01-02")
	done := false
	for page := 1; page <= maxSearchPages && !done; page++ {
		res, err := ws.API.SearchRecent(ctx, query, page, 100)
		if err != nil {
			if slack.IsCode(err, "missing_scope", "not_allowed_token_type", "invalid_arg_name") {
				p.searchOff = "off (" + err.Error() + "); cold threads are re-read in turn instead"
				p.log.Warn("search unavailable; tracked threads will be re-read one by one", "err", err)
			}
			return err
		}
		for i := range res.Matches {
			m := &res.Matches[i]
			if slack.TSTime(m.TS).Before(lower) {
				done = true
				break
			}
			key := m.Channel.ID + "/" + m.ThreadTS()
			if t := p.threads[key]; t != nil && !t.gone && !now.Before(t.hotUntil) {
				t.hotUntil, t.next = now.Add(hotFor), time.Time{}
			}
		}
		if res.Pages <= page || len(res.Matches) == 0 {
			done = true
		}
	}
	if !done {
		p.log.Debug("search sweep hit its page limit; older replies are left to the round-robin reads")
	}
	p.searchCov = now.Add(-searchLag)
	return nil
}

func (p *Poller) count(r routed) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch r {
	case started:
		p.status.Sessions++
	case steered:
		p.status.Replies++
	}
}

func (p *Poller) activeSince() map[string]time.Time {
	if p.ActiveSince == nil {
		return nil
	}
	return p.ActiveSince()
}

// bucket is a token bucket refilled at perMin per minute, holding up to 10 s worth (min 2).
type bucket struct {
	rate, cap, tokens float64
	last              time.Time
}

func newBucket(perMin float64, now time.Time) bucket {
	c := max(perMin/6, 2)
	return bucket{rate: perMin / 60, cap: c, tokens: c, last: now}
}

func (b *bucket) level(now time.Time) float64 {
	b.tokens = min(b.cap, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
	return b.tokens
}

func (b *bucket) take(now time.Time) bool {
	if b.level(now) < 1 {
		return false
	}
	b.tokens--
	return true
}

func tsAfter(a, b string) bool { return slack.TSTime(a).After(slack.TSTime(b)) }

func maxTS(a, b string) string {
	if tsAfter(b, a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
