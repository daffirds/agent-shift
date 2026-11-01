package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daffirds/agent-shift/internal/config"
	"github.com/daffirds/agent-shift/internal/engine"
	"github.com/daffirds/agent-shift/internal/slack"
	"github.com/daffirds/agent-shift/internal/tools"
)

// Workspace is a connected Slack workspace as seen by sessions.
type Workspace struct {
	TeamID   string
	Name     string
	UserID   string
	UserName string
	API      *slack.Client
	Dir      *slack.Directory

	appID atomic.Value // string; our Slack app's ID, learned from incoming events
}

func (w *Workspace) AppID() string {
	v, _ := w.appID.Load().(string)
	return v
}

func (w *Workspace) SetAppID(id string) {
	if id != "" {
		w.appID.Store(id)
	}
}

// Manager owns all sessions and routes Slack events to them.
type Manager struct {
	log     *slog.Logger
	host    *tools.Host
	baseURL string
	engines map[string]engine.Engine
	cfg     atomic.Pointer[config.Config]
	limit   limiter

	ctx context.Context

	mu       sync.Mutex
	ws       map[string]*Workspace
	sessions map[string]*Session // live sessions by ID
	byThread map[string]*Session // team/channel/thread_ts
	byMsg    map[string]*Session // team/channel/ts of messages inside tracked threads

	interrupted []*Session // were working when the daemon last stopped

	ownMsgs   *ttlSet // team/channel/ts posted by agent-shift
	ownFiles  *ttlSet // file IDs uploaded by agent-shift
	ownReacts *ttlSet // team/channel/ts/emoji changed by agent-shift
	seen      *ttlSet // dedupe of delivered events
}

type Options struct {
	Log     *slog.Logger
	Host    *tools.Host
	BaseURL string // http://127.0.0.1:port, where Host is mounted
	Engines map[string]engine.Engine
}

func NewManager(o Options) *Manager {
	return &Manager{
		log: o.Log, host: o.Host, baseURL: o.BaseURL, engines: o.Engines,
		ws:       map[string]*Workspace{},
		sessions: map[string]*Session{},
		byThread: map[string]*Session{},
		byMsg:    map[string]*Session{},
		ownMsgs:  newTTLSet(24 * time.Hour), ownFiles: newTTLSet(24 * time.Hour),
		ownReacts: newTTLSet(10 * time.Minute), seen: newTTLSet(time.Hour),
	}
}

func threadKey(team, channel, ts string) string { return team + "/" + channel + "/" + ts }

// SetConfig applies a (re)loaded configuration.
func (m *Manager) SetConfig(cfg *config.Config) {
	m.cfg.Store(cfg)
	m.limit.setMax(cfg.Daemon.MaxSessions)
}

func (m *Manager) Config() *config.Config { return m.cfg.Load() }

// SetWorkspace registers (or replaces) a connected workspace.
func (m *Manager) SetWorkspace(w *Workspace) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old := m.ws[w.TeamID]; old != nil {
		w.SetAppID(old.AppID())
	}
	m.ws[w.TeamID] = w
	for _, s := range m.sessions {
		if s.rec.TeamID == w.TeamID {
			s.wsp.Store(w)
		}
	}
}

func (m *Manager) RemoveWorkspace(teamID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.ws, teamID)
}

// Start restores persisted sessions (as sleeping) and starts housekeeping.
func (m *Manager) Start(ctx context.Context) error {
	m.ctx = ctx
	recs, err := LoadRecords()
	if err != nil {
		return err
	}
	ttl := m.cfg.Load().Daemon.ThreadTTL.Std()
	m.mu.Lock()
	for _, r := range recs {
		if !r.State.Live() {
			continue
		}
		if time.Since(r.LastActive) > ttl {
			r.State = StateEnded
			_ = r.save()
			continue
		}
		wasWorking := r.State == StateRunning || r.State == StateQueued
		if r.State != StateFailed {
			r.State = StateSleeping
		}
		_ = r.save()
		s := m.newSessionLocked(r)
		if wasWorking && time.Since(r.LastActive) < 6*time.Hour {
			m.interrupted = append(m.interrupted, s)
		}
		m.log.Debug("restored session", "session", r.ID, "agent", r.Agent)
	}
	m.mu.Unlock()
	go m.janitor(ctx)
	return nil
}

// ResumeInterrupted continues sessions that were mid-task when the daemon last stopped.
// Call it once workspaces are connected.
func (m *Manager) ResumeInterrupted() {
	m.mu.Lock()
	list := m.interrupted
	m.interrupted = nil
	m.mu.Unlock()
	for _, s := range list {
		s.log.Info("resuming work interrupted by a restart")
		s.deliver(inbound{restarted: true})
	}
}

// newSessionLocked creates the runtime for a record and indexes it. m.mu must be held.
func (m *Manager) newSessionLocked(r *Record) *Session {
	s := &Session{
		m: m, rec: *r,
		log:   m.log.With("session", r.ID, "agent", r.Agent),
		inbox: make(chan inbound, 64),
		done:  make(chan struct{}),
	}
	s.wsp.Store(m.ws[r.TeamID])
	s.ctx, s.cancel = context.WithCancel(m.ctx)
	m.sessions[r.ID] = s
	m.byThread[threadKey(r.TeamID, r.Channel, r.ThreadTS)] = s
	for _, ts := range r.Known {
		m.byMsg[threadKey(r.TeamID, r.Channel, ts)] = s
	}
	go s.run()
	return s
}

func (m *Manager) janitor(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ttl := m.cfg.Load().Daemon.ThreadTTL.Std()
		m.mu.Lock()
		var expired []*Session
		for _, s := range m.sessions {
			r := s.snapshot()
			if (r.State == StateSleeping || r.State == StateFailed) && time.Since(r.LastActive) > ttl {
				expired = append(expired, s)
			}
		}
		m.mu.Unlock()
		for _, s := range expired {
			s.log.Info("thread expired; no longer listening", "idle_for", time.Since(s.snapshot().LastActive).Round(time.Minute))
			m.end(s)
		}
	}
}

// evictIdle asks the least recently active idle session (other than except) to stop its
// CLI process, freeing a slot. It reports whether such a session was found.
func (m *Manager) evictIdle(except *Session) bool {
	m.mu.Lock()
	var victim *Session
	var oldest time.Time
	for _, s := range m.sessions {
		if s == except {
			continue
		}
		if r := s.snapshot(); r.State == StateIdle && (victim == nil || r.LastActive.Before(oldest)) {
			victim, oldest = s, r.LastActive
		}
	}
	m.mu.Unlock()
	if victim == nil {
		return false
	}
	victim.deliver(inbound{evict: true})
	return true
}

// end stops a session for good and forgets its thread.
func (m *Manager) end(s *Session) {
	s.stop()
	m.mu.Lock()
	delete(m.sessions, s.rec.ID)
	delete(m.byThread, threadKey(s.rec.TeamID, s.rec.Channel, s.rec.ThreadTS))
	for k, v := range m.byMsg {
		if v == s {
			delete(m.byMsg, k)
		}
	}
	m.mu.Unlock()
}

// HandleEvent routes one Slack event. It may call the Slack API, so callers should
// invoke it from a worker (one per workspace keeps per-thread ordering), not the socket loop.
func (m *Manager) HandleEvent(ctx context.Context, ev *slack.Event, appID string) {
	m.mu.Lock()
	ws := m.ws[ev.TeamID]
	m.mu.Unlock()
	if ws == nil {
		return
	}
	ws.SetAppID(appID)
	if ev.EventID != "" && !m.seen.addNew("ev/"+ev.EventID) {
		return
	}
	switch {
	case ev.Message != nil:
		m.handleMessage(ctx, ws, ev.Message, nil)
	case ev.Reaction != nil:
		m.handleReaction(ctx, ws, ev.Reaction)
	}
}

// isOwn reports whether a message was posted by agent-shift (as opposed to typed by the user).
func (m *Manager) isOwn(ws *Workspace, msg *slack.Message) bool {
	if msg.HasBlockID(slack.FooterBlockID) || m.ownMsgs.has(threadKey(ws.TeamID, msg.Channel, msg.TS)) {
		return true
	}
	if id := ws.AppID(); id != "" && msg.AppID == id {
		return true
	}
	if len(msg.Files) > 0 {
		for _, f := range msg.Files {
			if !m.ownFiles.has(f.ID) {
				return false
			}
		}
		return true
	}
	return false
}

// routed is what handleMessage did with a message.
type routed int

const (
	ignored routed = iota
	started        // started a new session
	steered        // delivered to its thread's session
)

// handleMessage routes a message: a reply steers its thread's session, a top-level message may
// start one. eligible, if set, limits which agents may start it (used by catch-up).
func (m *Manager) handleMessage(ctx context.Context, ws *Workspace, msg *slack.Message, eligible func(*config.Agent) bool) routed {
	if !msg.IsNewMessage() || systemUsers[msg.User] {
		return ignored
	}
	if !m.seen.addNew("msg/" + threadKey(ws.TeamID, msg.Channel, msg.TS)) {
		return ignored
	}
	own := m.isOwn(ws, msg)

	if msg.IsThreadReply() {
		m.mu.Lock()
		s := m.byThread[threadKey(ws.TeamID, msg.Channel, msg.ThreadTS)]
		if s != nil {
			m.byMsg[threadKey(ws.TeamID, msg.Channel, msg.TS)] = s
		}
		m.mu.Unlock()
		if s == nil || s.knows(msg.TS) { // knows: already handled, e.g. before a restart
			return ignored
		}
		s.remember(msg.TS)
		if own {
			return ignored
		}
		if from := s.agent().ThreadFrom; len(from) > 0 && !senderIn(from, ws, msg) {
			s.log.Debug("ignoring reply from sender outside thread_from", "sender", msg.SenderID())
			return ignored
		}
		s.deliver(inbound{msg: msg, late: lateBy(msg.TS) > 0})
		return steered
	}

	if own {
		return ignored
	}
	m.mu.Lock()
	tracked := m.byThread[threadKey(ws.TeamID, msg.Channel, msg.TS)] != nil
	m.mu.Unlock()
	if tracked {
		return ignored // the thread already has its session
	}
	cfg := m.cfg.Load()
	text := slack.Render(ctx, ws.Dir, msg)
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if a.Disabled || (eligible != nil && !eligible(a)) || !matchTrigger(&a.Trigger, ws, msg, text) {
			continue
		}
		m.startSession(ctx, ws, a, msg, text)
		return started // first matching agent wins; one thread, one session
	}
	return ignored
}

func (m *Manager) handleReaction(ctx context.Context, ws *Workspace, r *slack.ReactionEvent) {
	if r.User == ws.UserID && m.ownReacts.has(threadKey(ws.TeamID, r.Channel, r.TS)+"/"+r.Name) {
		return // our own status/tool reaction echoing back
	}
	key := threadKey(ws.TeamID, r.Channel, r.TS)
	m.mu.Lock()
	s := m.byMsg[key]
	if s == nil {
		s = m.byThread[key]
	}
	channelHasSessions := false
	if s == nil {
		for _, c := range m.sessions {
			if c.rec.TeamID == ws.TeamID && c.rec.Channel == r.Channel {
				channelHasSessions = true
				break
			}
		}
	}
	m.mu.Unlock()

	var target *slack.Message
	if s == nil && channelHasSessions {
		// A reaction on a reply we haven't seen (e.g. from before a restart): look it up.
		msg, err := ws.API.GetMessage(ctx, r.Channel, r.TS)
		if err != nil || !msg.IsThreadReply() {
			return
		}
		target = msg
		m.mu.Lock()
		if s = m.byThread[threadKey(ws.TeamID, r.Channel, msg.ThreadTS)]; s != nil {
			m.byMsg[key] = s
		}
		m.mu.Unlock()
	}
	if s == nil {
		return
	}
	if from := s.agent().ThreadFrom; len(from) > 0 && !userIn(from, ws, r.User) {
		return
	}
	s.deliver(inbound{reaction: r, target: target})
}

func (m *Manager) startSession(ctx context.Context, ws *Workspace, a *config.Agent, msg *slack.Message, text string) {
	convName := ws.Dir.ConversationName(ctx, msg.Channel)
	permalink, _ := ws.API.Permalink(ctx, msg.Channel, msg.TS)
	now := time.Now()
	r := &Record{
		ID: newID(), Agent: a.Name, AgentConfig: *a, Engine: a.Engine,
		TeamID: ws.TeamID, Channel: msg.Channel, ChannelName: convName, ThreadTS: msg.TS,
		Permalink: permalink, From: ws.Dir.SenderName(ctx, msg), Title: slack.Excerpt(text, 80),
		Workdir: a.Workdir, State: StateQueued, CreatedAt: now, LastActive: now, Known: []string{msg.TS},
	}
	log := m.log.With("session", r.ID, "agent", a.Name)
	var earlier []slack.Message
	if msg.ReplyCount > 0 { // picked up late (catch-up after downtime): people may have replied already
		page, err := ws.API.Replies(ctx, msg.Channel, msg.TS, slack.HistoryOptions{Limit: 200})
		if err != nil {
			log.Warn("cannot read the thread's earlier replies", "err", err)
		} else {
			for i := range page.Messages {
				e := &page.Messages[i]
				if e.TS == msg.TS {
					continue
				}
				e.Channel = msg.Channel
				r.Known = append(r.Known, e.TS) // handled here, never delivered again
				if e.IsNewMessage() && !m.isOwn(ws, e) && (len(a.ThreadFrom) == 0 || senderIn(a.ThreadFrom, ws, e)) {
					earlier = append(earlier, *e)
				}
			}
		}
	}
	if a.Worktree {
		root, dir, err := createWorktree(ctx, a.Workdir, r.ID)
		if err != nil {
			log.Warn("worktree failed; using the workdir directly", "err", err)
		} else {
			r.Worktree, r.Workdir = root, dir
		}
	}
	if err := r.save(); err != nil {
		log.Error("cannot persist session", "err", err)
		return
	}
	log.Info("new session", "conversation", convName, "from", r.From, "engine", a.Engine, "title", r.Title)
	m.mu.Lock()
	s := m.newSessionLocked(r)
	m.mu.Unlock()
	s.deliver(inbound{prompt: triggerPrompt(ctx, ws, r, msg, text, earlier)})
}

// Snapshot returns all sessions (live and persisted), newest first.
func (m *Manager) Snapshot() []Record {
	recs, _ := LoadRecords()
	m.mu.Lock()
	live := make(map[string]*Session, len(m.sessions))
	for id, s := range m.sessions {
		live[id] = s
	}
	m.mu.Unlock()
	out := make([]Record, 0, len(recs))
	for _, r := range recs {
		if s := live[r.ID]; s != nil {
			out = append(out, s.snapshot())
		} else {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

var ErrNoSession = errors.New("no such live session")

func (m *Manager) find(prefix string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var hit *Session
	for id, s := range m.sessions {
		if id == prefix {
			return s, nil
		}
		if strings.HasPrefix(id, prefix) {
			if hit != nil {
				return nil, fmt.Errorf("session id %q is ambiguous", prefix)
			}
			hit = s
		}
	}
	if hit == nil {
		return nil, ErrNoSession
	}
	return hit, nil
}

// Stop ends a session: its process stops and its thread no longer steers it.
func (m *Manager) Stop(id string) error {
	s, err := m.find(id)
	if err != nil {
		return err
	}
	m.end(s)
	return nil
}

// Interrupt cancels the session's current turn.
func (m *Manager) Interrupt(id string) error {
	s, err := m.find(id)
	if err != nil {
		return err
	}
	s.deliver(inbound{interrupt: true})
	return nil
}

// Shutdown stops every process (sessions stay resumable).
func (m *Manager) Shutdown() {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range all {
		wg.Go(func() { s.suspend() })
	}
	wg.Wait()
}

// Counts summarises sessions by state for status output.
func (m *Manager) Counts() map[State]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[State]int{}
	for _, s := range m.sessions {
		out[s.snapshot().State]++
	}
	return out
}

func binaryPath(cfg *config.Config, eng string) config.Binary {
	if eng == config.EngineCodex {
		return cfg.Engines.Codex
	}
	return cfg.Engines.Claude
}
