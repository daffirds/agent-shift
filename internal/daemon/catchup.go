package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"sync"
	"time"

	"github.com/daffirds/agent-shift/internal/app"
	"github.com/daffirds/agent-shift/internal/config"
	"github.com/daffirds/agent-shift/internal/session"
)

// Catch-up after downtime. Socket Mode only delivers events while connected, and Slack gives up
// on undelivered ones after a few retries, so whatever was posted while the computer was off or
// asleep would be lost. Each workspace therefore keeps a watermark (state.json): the time up to
// which its events were received. Whenever the link comes back (daemon start, reconnect, wake
// from sleep) the session manager replays Slack history from there.

const (
	heartbeatEvery = 30 * time.Second
	// pauseAfter: a heartbeat this late means the process was suspended (sleep, hibernation).
	pauseAfter = 2 * time.Minute
	// catchUpMargin re-reads a little before the watermark: a dead link takes up to ~50s to
	// notice (ping interval + timeout), and Slack's clock may differ from ours.
	catchUpMargin = 3 * time.Minute
	catchUpRetry  = 2 * time.Minute
)

// CatchUpStatus describes a workspace's last catch-up.
type CatchUpStatus struct {
	At   time.Time `json:"at"`
	From time.Time `json:"from"` // since when events had not been received
	session.CatchUpResult
	Error string `json:"error,omitempty"`
}

// syncState is state.json. Per workspace: until when Slack events were received (the
// watermark). Per agent: since when it has been enabled, so a catch-up only replays what the
// agent would have seen live; one added while the daemon was stopped starts fresh.
type syncState struct {
	mu     sync.Mutex
	path   string
	synced map[string]time.Time // team ID → watermark
	agents map[string]time.Time // agent name → enabled since
}

type stateFile struct {
	Synced map[string]time.Time `json:"synced"`
	Agents map[string]time.Time `json:"agents"`
}

func loadState() *syncState {
	s := &syncState{path: app.StatePath(), synced: map[string]time.Time{}, agents: map[string]time.Time{}}
	var f stateFile
	if data, err := os.ReadFile(s.path); err == nil && json.Unmarshal(data, &f) == nil {
		if f.Synced != nil {
			s.synced = f.Synced
		}
		if f.Agents != nil {
			s.agents = f.Agents
		}
	}
	return s
}

func (s *syncState) get(team string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.synced[team]
}

func (s *syncState) set(team string, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.synced[team] = t.Round(0)
	return s.saveLocked()
}

func (s *syncState) forget(team string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.synced[team]; ok {
		delete(s.synced, team)
		_ = s.saveLocked()
	}
}

// trackAgents notes when each enabled agent was first seen, and forgets disabled or removed ones.
func (s *syncState) trackAgents(cfg *config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Round(0)
	enabled := map[string]bool{}
	changed := false
	for _, a := range cfg.Agents {
		if a.Disabled {
			continue
		}
		enabled[a.Name] = true
		if _, ok := s.agents[a.Name]; !ok {
			s.agents[a.Name] = now
			changed = true
		}
	}
	for name := range s.agents {
		if !enabled[name] {
			delete(s.agents, name)
			changed = true
		}
	}
	if changed {
		_ = s.saveLocked()
	}
}

func (s *syncState) agentsSince() map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.agents)
}

func (s *syncState) saveLocked() error {
	data, _ := json.MarshalIndent(stateFile{Synced: s.synced, Agents: s.agents}, "", "  ")
	return config.WriteFileAtomic(s.path, data, 0o600)
}

// linkUp runs when a workspace's link says hello, before it counts as connected: everything
// since the watermark may have been missed.
func (d *daemon) linkUp(c *connection) {
	team := c.ws.TeamID
	switch from := d.marks.get(team); {
	case from.IsZero(): // first connection: nothing to catch up on
		_ = d.marks.set(team, time.Now())
		c.syncing.Store(false)
	case d.mgr.Config().Daemon.CatchUpWindow() <= 0:
		c.syncing.Store(false)
	default:
		c.requestCatchUp(from)
	}
}

// requestCatchUp queues a replay from `from` on the workspace's event worker and freezes the
// watermark until it succeeds.
func (c *connection) requestCatchUp(from time.Time) {
	c.syncing.Store(true)
	c.catchMu.Lock()
	if c.catchFrom.IsZero() || from.Before(c.catchFrom) {
		c.catchFrom = from
	}
	c.catchMu.Unlock()
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// heartbeat advances the workspace's watermark while its link is healthy, and notices when the
// computer was asleep: a link that predates the pause may have silently missed events, so it is
// reopened, which triggers a catch-up.
func (d *daemon) heartbeat(ctx context.Context, c *connection, log *slog.Logger) {
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	last := time.Now().Round(0) // wall clock: the monotonic clock may stand still during sleep
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now().Round(0)
		prev := last
		last = now
		if gap := now.Sub(prev); gap > pauseAfter {
			if up, _ := c.since.Load().(time.Time); c.connected.Load() && up.Before(prev) {
				log.Info("resumed after a pause; reconnecting to catch up", "paused", gap.Round(time.Second))
				c.syncing.Store(true)
				c.sock.Reconnect("resumed after a pause")
			}
			continue
		}
		d.advance(c, now)
	}
}

// advance moves the watermark to now if the link is up and nothing is waiting to be replayed.
func (d *daemon) advance(c *connection, now time.Time) {
	if c.connected.Load() && !c.syncing.Load() {
		if err := d.marks.set(c.ws.TeamID, now); err != nil {
			d.log.Debug("cannot save state", "err", err)
		}
	}
}

// runCatchUp replays the pending window. It runs on the workspace's event worker, so live
// events wait until the older, replayed ones are routed.
func (d *daemon) runCatchUp(ctx context.Context, c *connection, log *slog.Logger) {
	c.catchMu.Lock()
	from := c.catchFrom
	c.catchFrom = time.Time{}
	c.catchMu.Unlock()
	if from.IsZero() {
		return
	}
	now := time.Now().Round(0)
	window := d.mgr.Config().Daemon.CatchUpWindow()
	if window <= 0 {
		c.syncing.Store(false)
		return
	}
	since := from.Add(-catchUpMargin)
	if since.Before(now.Add(-window)) {
		log.Warn("offline for longer than daemon.catch_up; older messages are not replayed",
			"offline_since", from.Format(time.DateTime), "catch_up", config.FormatDuration(window))
		since = now.Add(-window)
	}
	offline := now.Sub(from).Round(time.Second)
	rctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	res, err := d.mgr.CatchUp(rctx, c.ws.TeamID, since, d.marks.agentsSince())
	if ctx.Err() != nil {
		return
	}
	st := &CatchUpStatus{At: now, From: from, CatchUpResult: res}
	if err != nil {
		st.Error = err.Error()
		c.catchUp.Store(st)
		log.Warn("catch-up incomplete; retrying", "err", err, "in", catchUpRetry)
		time.AfterFunc(catchUpRetry, func() { c.requestCatchUp(from) })
		return
	}
	c.catchUp.Store(st)
	level := slog.LevelDebug
	if res.Sessions+res.Replies > 0 || offline > 5*time.Minute {
		level = slog.LevelInfo
	}
	log.Log(ctx, level, "caught up on Slack activity missed while offline", "offline", offline,
		"conversations", res.Conversations, "threads", res.Threads, "new_sessions", res.Sessions, "replies", res.Replies)
	c.catchMu.Lock()
	pending := !c.catchFrom.IsZero()
	c.catchMu.Unlock()
	if !pending {
		_ = d.marks.set(c.ws.TeamID, now)
		c.syncing.Store(false)
	}
}
