package session

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daffirds/agent-shift/internal/config"
	"github.com/daffirds/agent-shift/internal/engine"
	"github.com/daffirds/agent-shift/internal/slack"
	"github.com/daffirds/agent-shift/internal/tools"
)

// inbound is one thing to deliver to the agent.
type inbound struct {
	prompt    string // prebuilt prompt (the trigger message)
	msg       *slack.Message
	late      bool // msg was posted while the daemon was offline (replayed by a catch-up)
	reaction  *slack.ReactionEvent
	target    *slack.Message // the reacted-to message, if already fetched
	interrupt bool
	evict     bool // another session needs a process slot: stop the CLI if idle
	restarted bool // the daemon restarted while this session was working
}

// Session is the runtime of one thread ↔ CLI conversation. Its goroutine (run) owns the process.
// It never writes to Slack itself: everything people see comes from the agent's tool calls.
type Session struct {
	m      *Manager
	wsp    atomic.Pointer[Workspace]
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	inbox  chan inbound
	done   chan struct{}
	ending atomic.Bool

	mu  sync.Mutex
	rec Record

	// Owned by run().
	proc       engine.Session
	pending    []string // prompts not delivered yet because the CLI failed to start
	mcpPath    string
	slot       bool
	busy       bool
	stopping   bool // we asked the CLI to interrupt the current turn
	transcript *os.File
}

func (s *Session) ws() *Workspace { return s.wsp.Load() }

func (s *Session) snapshot() Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rec
	r.Posted, r.Known = slices.Clone(r.Posted), slices.Clone(r.Known)
	return r
}

func (s *Session) update(fn func(r *Record)) {
	s.mu.Lock()
	fn(&s.rec)
	r := s.rec
	s.mu.Unlock()
	if err := r.save(); err != nil {
		s.log.Warn("cannot persist session", "err", err)
	}
}

func (s *Session) setState(st State, errText string) {
	s.update(func(r *Record) {
		r.State = st
		if errText != "" || st == StateRunning {
			r.LastError = errText
		}
	})
}

// agent returns the agent's current config, or the snapshot if it was removed.
func (s *Session) agent() *config.Agent {
	if cfg := s.m.cfg.Load(); cfg != nil {
		if a := cfg.Agent(s.rec.Agent); a != nil {
			return a
		}
	}
	a := s.rec.AgentConfig
	return &a
}

func (s *Session) deliver(in inbound) {
	select {
	case s.inbox <- in:
	case <-s.ctx.Done():
	case <-time.After(10 * time.Second):
		s.log.Warn("session inbox full; dropping event")
	}
}

// knows reports whether a message of the thread was already seen.
func (s *Session) knows(ts string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.rec.Known, ts)
}

// remember indexes a message ts of the thread (reactions on it route here).
func (s *Session) remember(ts string) {
	s.update(func(r *Record) {
		if !slices.Contains(r.Known, ts) {
			r.Known = append(r.Known, ts)
			if len(r.Known) > 1000 {
				r.Known = r.Known[len(r.Known)-1000:]
			}
		}
		r.LastActive = time.Now()
	})
}

// --- tools.Echo ---

func (s *Session) PostedMessage(channel, threadTS, ts string) {
	s.m.ownMsgs.add(threadKey(s.rec.TeamID, channel, ts))
	s.update(func(r *Record) { r.Posted = append(r.Posted, channel+"/"+ts) })
	if channel == s.rec.Channel && threadTS == s.rec.ThreadTS {
		s.m.mu.Lock()
		s.m.byMsg[threadKey(s.rec.TeamID, channel, ts)] = s
		s.m.mu.Unlock()
		s.remember(ts)
	}
}

func (s *Session) UploadedFile(id string) { s.m.ownFiles.add(id) }

func (s *Session) Reacted(channel, ts, emoji string) {
	s.m.ownReacts.add(threadKey(s.rec.TeamID, channel, ts) + "/" + emoji)
}

func (s *Session) Owns(channel, ts string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.rec.Posted, channel+"/"+ts)
}

// --- lifecycle ---

// stop ends the session for good.
func (s *Session) stop() {
	s.ending.Store(true)
	s.cancel()
	<-s.done
}

// suspend stops the process but keeps the session resumable (daemon shutdown).
func (s *Session) suspend() {
	s.cancel()
	<-s.done
}

func (s *Session) run() {
	defer close(s.done)
	idle := time.NewTimer(time.Hour)
	idle.Stop()
	var (
		events <-chan engine.Event
		exited <-chan struct{}
	)
	for {
		select {
		case <-s.ctx.Done():
			st := s.snapshot().State
			s.closeProcess()
			switch {
			case s.ending.Load():
				s.setState(StateEnded, "")
			case st == StateRunning || st == StateQueued:
				// Daemon shutdown mid-work: keep the state so the next start resumes the work.
			case st != StateFailed:
				s.setState(StateSleeping, "")
			}
			return

		case in := <-s.inbox:
			batch := []inbound{in}
			for drained := false; !drained; {
				select {
				case more := <-s.inbox:
					batch = append(batch, more)
				default:
					drained = true
				}
			}
			if slices.ContainsFunc(batch, func(in inbound) bool { return in.evict }) && s.proc != nil && !s.busy {
				s.log.Info("idle; stopping agent process to free a slot (resumes on next message)")
				s.closeProcess()
				events, exited = nil, nil
				idle.Stop()
				s.setState(StateSleeping, "")
			}
			prompts := s.prepare(batch)
			if len(prompts) == 0 {
				continue
			}
			if s.proc == nil && len(s.pending) > 0 {
				prompts, s.pending = append(s.pending, prompts...), nil
			}
			var prefix string
			if s.proc == nil {
				p, err := s.startProcess()
				if err != nil {
					if s.ctx.Err() == nil {
						s.pending = append(s.pending, prompts...) // retried with the next thread message
						if len(s.pending) > 20 {
							s.pending = s.pending[len(s.pending)-20:]
						}
						s.log.Error("cannot start session; retrying with the next thread message", "err", err)
						s.setState(StateFailed, err.Error())
					}
					continue
				}
				prefix = p
				events, exited = s.proc.Events(), s.proc.Done()
			}
			idle.Stop()
			s.busy = true
			for i, p := range prompts {
				if i == 0 {
					p = prefix + p
				}
				if err := s.proc.Send(s.ctx, p); err != nil {
					s.log.Warn("deliver failed", "err", err)
				}
			}
			s.update(func(r *Record) { r.LastActive = time.Now() })

		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			s.onEvent(ev, idle)

		case <-exited:
			err := s.proc.Err()
			wasBusy := s.busy
			s.releaseProcess()
			events, exited = nil, nil
			idle.Stop()
			if err != nil {
				s.log.Warn("agent process exited", "err", err, "mid_turn", wasBusy)
			}
			s.setState(StateSleeping, errString(err))

		case <-idle.C:
			if s.proc != nil && !s.busy {
				s.log.Info("idle; stopping agent process (resumes on next message)")
				s.closeProcess()
				events, exited = nil, nil
				s.setState(StateSleeping, "")
			}
		}
	}
}

// prepare turns inbound events into prompts, handling interrupts on the way.
func (s *Session) prepare(batch []inbound) []string {
	if s.ws() == nil {
		s.log.Warn("workspace not connected; dropping input")
		return nil
	}
	var prompts []string
	for _, in := range batch {
		switch {
		case in.interrupt:
			s.interrupt("interrupted from the CLI")
		case in.restarted:
			if s.snapshot().EngineID == "" {
				// Never got to start: the triggering message is the thread's first message.
				rec := s.snapshot()
				prompts = append(prompts, resumeContext(s.ctx, s.ws(), &rec)+
					"The first message of this thread matched your trigger; treat it as your task.")
			} else {
				prompts = append(prompts, "agent-shift (the program relaying Slack to you) was restarted while you were working on this thread. "+
					"Continue the task where you left off; use slack_read_thread if you need to catch up.")
			}
		case in.prompt != "":
			prompts = append(prompts, in.prompt)
		case in.msg != nil:
			prompts = append(prompts, replyPrompt(s.ctx, s.ws(), in.msg, in.late))
		case in.reaction != nil:
			r := in.reaction
			if !r.Added {
				continue // removals are noise for the agent
			}
			target := in.target
			if target == nil {
				ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
				target, _ = s.ws().API.GetMessage(ctx, r.Channel, r.TS)
				cancel()
			}
			prompts = append(prompts, reactionPrompt(s.ctx, s.ws(), r, target, s.Owns(r.Channel, r.TS)))
		}
	}
	return prompts
}

func (s *Session) interrupt(why string) {
	if s.proc == nil || !s.busy {
		return
	}
	s.log.Info("interrupting turn", "why", why)
	s.stopping = true
	ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
	defer cancel()
	if err := s.proc.Interrupt(ctx); err != nil {
		s.stopping = false
		s.log.Warn("interrupt failed", "err", err)
	}
}

func (s *Session) onEvent(ev engine.Event, idle *time.Timer) {
	switch ev.Kind {
	case engine.TurnStarted:
		s.busy = true
		idle.Stop()
		s.update(func(r *Record) {
			r.State, r.LastError, r.LastActive = StateRunning, "", time.Now()
			r.Turns++
		})
	case engine.TurnCompleted:
		s.busy = false
		requested := s.stopping
		s.stopping = false
		s.update(func(r *Record) { r.State, r.LastActive = StateIdle, time.Now() })
		if requested && ev.Error != "" {
			s.log.Info("turn interrupted", "detail", ev.Error)
		} else if ev.Error != "" && !strings.Contains(ev.Error, "interrupted") {
			s.log.Warn("turn failed", "err", ev.Error)
			s.update(func(r *Record) { r.LastError = ev.Error })
		}
		idle.Reset(s.m.cfg.Load().Daemon.IdleTimeout.Std())
	case engine.AssistantText:
		s.log.Debug("assistant", "text", oneLine(ev.Text, 200))
	case engine.ToolCall:
		s.log.Debug("tool", "call", oneLine(ev.Text, 200))
	case engine.Notice:
		s.log.Warn(ev.Text)
	}
}

// startProcess acquires a slot and launches (or resumes) the CLI. It returns a prefix for the
// first prompt when the CLI conversation had to be rebuilt from Slack history.
func (s *Session) startProcess() (string, error) {
	if !s.m.limit.tryAcquire() {
		// Idle CLI processes only wait for thread replies; suspend one (it resumes on demand)
		// rather than make new work wait behind it.
		if !s.m.evictIdle(s) {
			s.setState(StateQueued, "")
			s.log.Info("waiting for a free session slot", "max_sessions", s.m.cfg.Load().Daemon.MaxSessions)
		}
		granted, cancel := s.m.limit.wait()
		select {
		case <-granted:
		case <-s.ctx.Done():
			cancel()
			return "", s.ctx.Err()
		}
	}
	s.slot = true
	if s.ws() == nil {
		s.releaseProcess()
		return "", fmt.Errorf("workspace %s is not connected", s.rec.TeamID)
	}

	a := s.agent()
	cfg := s.m.cfg.Load()
	eng := s.m.engines[a.Engine]
	if eng == nil {
		s.releaseProcess()
		return "", fmt.Errorf("unknown engine %q", a.Engine)
	}
	bin, err := engine.Find(a.Engine, binaryPath(cfg, a.Engine).Path)
	if err != nil {
		s.releaseProcess()
		return "", fmt.Errorf("%s CLI not found (install it or set engines.%s.path): %w", a.Engine, a.Engine, err)
	}
	rec := s.snapshot()
	if st, err := os.Stat(rec.Workdir); err != nil || !st.IsDir() {
		s.releaseProcess()
		return "", fmt.Errorf("working directory %s does not exist", rec.Workdir)
	}

	prefix := ""
	resume := rec.EngineID
	if resume != "" && a.Engine == config.EngineClaude && !claudeSessionExists(resume) {
		resume, prefix = "", resumeContext(s.ctx, s.ws(), &rec)
	}
	s.mcpPath = s.m.host.Register(&tools.Binding{
		Dir: s.ws().Dir, Self: s.ws().UserID, SelfName: s.ws().UserName,
		Channel: rec.Channel, ThreadTS: rec.ThreadTS, Workdir: rec.Workdir, FilesDir: rec.FilesDir(),
		Footer: a.FooterText(), Log: s.log, Echo: s,
	})
	if err := os.MkdirAll(rec.Dir(), 0o700); err == nil {
		s.transcript, _ = os.OpenFile(rec.TranscriptPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	}
	spec := engine.Spec{
		Binary: bin, Args: binaryPath(cfg, a.Engine).Args, Env: binaryPath(cfg, a.Engine).Env, ExtraArgs: a.ExtraArgs,
		Workdir: rec.Workdir, Instructions: instructions(a, s.ws(), &rec), Model: a.Model, Effort: a.Effort, Autonomy: a.Autonomy,
		MCP:     engine.MCPServer{Name: tools.ServerName, URL: s.m.baseURL + s.mcpPath},
		AddDirs: []string{rec.FilesDir()}, ResumeID: resume, StateDir: rec.Dir(),
		Log: s.log, Transcript: engine.NewTranscriptLog(s.transcript),
	}
	_ = os.MkdirAll(rec.FilesDir(), 0o700)
	p, err := eng.Start(s.ctx, spec)
	if err != nil && resume != "" {
		s.log.Warn("resume failed; starting fresh with the thread history", "err", err)
		spec.ResumeID, prefix = "", resumeContext(s.ctx, s.ws(), &rec)
		p, err = eng.Start(s.ctx, spec)
	}
	if err != nil {
		s.releaseProcess()
		return "", err
	}
	s.proc = p
	s.update(func(r *Record) { r.EngineID = p.ID() })
	s.log.Info("agent process started", "engine", a.Engine, "engine_session", p.ID(), "resumed", resume != "", "workdir", rec.Workdir)
	return prefix, nil
}

// closeProcess shuts the CLI down gracefully and frees its resources.
func (s *Session) closeProcess() {
	if s.proc != nil {
		_ = s.proc.Close()
	}
	s.releaseProcess()
}

func (s *Session) releaseProcess() {
	s.proc = nil
	s.busy, s.stopping = false, false
	if s.mcpPath != "" {
		s.m.host.Unregister(s.mcpPath)
		s.mcpPath = ""
	}
	if s.transcript != nil {
		_ = s.transcript.Close()
		s.transcript = nil
	}
	if s.slot {
		s.slot = false
		s.m.limit.release()
	}
}

// claudeSessionExists checks Claude Code's transcript store so --resume won't fail.
func claudeSessionExists(id string) bool {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".claude")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "projects", "*", id+".jsonl"))
	return len(matches) > 0
}

func oneLine(s string, n int) string { return slack.Excerpt(s, n) }

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
