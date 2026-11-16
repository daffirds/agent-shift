// Package daemon wires everything together: one Slack poller per workspace (one goroutine, so
// messages stay in order), the session manager, the MCP tool host and a local control API, all
// in a single long-running process.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofrs/flock"

	"github.com/daffirds/agent-shift/internal/app"
	"github.com/daffirds/agent-shift/internal/config"
	"github.com/daffirds/agent-shift/internal/engine"
	"github.com/daffirds/agent-shift/internal/engine/claude"
	"github.com/daffirds/agent-shift/internal/engine/codex"
	"github.com/daffirds/agent-shift/internal/link"
	"github.com/daffirds/agent-shift/internal/session"
	"github.com/daffirds/agent-shift/internal/slack"
	"github.com/daffirds/agent-shift/internal/tools"
)

var ErrAlreadyRunning = errors.New("another agent-shift daemon is already running")

type daemon struct {
	ctx     context.Context // lives as long as the daemon
	log     *slog.Logger
	mgr     *session.Manager
	marks   *syncState
	started time.Time
	token   string
	stop    context.CancelFunc

	mu    sync.Mutex
	conns map[string]*connection
}

// connection is one workspace's poller and its worker.
type connection struct {
	ws     *session.Workspace
	tokens string // fingerprint, to notice a new token
	cancel context.CancelFunc
	poller atomic.Pointer[session.Poller]
	err    atomic.Value // string; why the workspace cannot be polled at all
}

// Run runs the daemon in the foreground until ctx is cancelled or a shutdown is requested.
func Run(ctx context.Context, log *slog.Logger) error {
	if err := app.EnsureHome(); err != nil {
		return err
	}
	lock := flock.New(app.LockPath())
	ok, err := lock.TryLock()
	if err != nil {
		return fmt.Errorf("lock %s: %w", app.LockPath(), err)
	}
	if !ok {
		return ErrAlreadyRunning
	}
	defer lock.Unlock()

	cfg, err := config.Load()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config %s:\n%w", app.ConfigPath(), err)
	}
	for _, w := range cfg.Warnings() {
		log.Warn(w)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	baseURL := "http://" + ln.Addr().String()
	host := tools.NewHost(log.With("component", "mcp"))
	mgr := session.NewManager(session.Options{
		Log: log, Host: host, BaseURL: baseURL,
		Engines: map[string]engine.Engine{config.EngineClaude: claude.Engine{}, config.EngineCodex: codex.Engine{}},
	})
	mgr.SetConfig(cfg)

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	d := &daemon{ctx: ctx, log: log, mgr: mgr, marks: loadState(), started: time.Now(), token: randomHex(24), stop: stop, conns: map[string]*connection{}}
	d.marks.trackAgents(cfg)

	mux := http.NewServeMux()
	mux.Handle("/mcp/", host)
	d.routes(mux)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("control server stopped", "err", err)
			stop()
		}
	}()

	rt := &Runtime{PID: os.Getpid(), Addr: ln.Addr().String(), Token: d.token, Started: d.started, Version: app.Version}
	if err := writeRuntime(rt); err != nil {
		return err
	}
	defer os.Remove(app.RuntimePath())

	if err := mgr.Start(ctx); err != nil {
		return err
	}
	d.reconcile(cfg)
	go d.heartbeat(ctx)
	mgr.ResumeInterrupted()
	log.Info("agent-shift daemon running", "version", app.Version, "pid", rt.PID, "agents", len(cfg.Agents), "workspaces", len(cfg.Workspaces))
	if len(cfg.Workspaces) == 0 || len(cfg.Agents) == 0 {
		log.Warn("nothing to do yet: run `agent-shift ui` to connect Slack and create an agent")
	}

	<-ctx.Done()
	log.Info("shutting down")
	d.saveMarks()
	d.mu.Lock()
	for _, c := range d.conns {
		c.cancel()
	}
	d.mu.Unlock()
	mgr.Shutdown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return nil
}

// reconcile starts, restarts or stops workspace pollers to match the config.
func (d *daemon) reconcile(cfg *config.Config) {
	d.mu.Lock()
	defer d.mu.Unlock()
	want := map[string]bool{}
	for _, w := range cfg.Workspaces {
		want[w.TeamID] = true
		token, err := link.Token(w.TeamID)
		fp := fingerprint(token + "|" + w.UserID)
		if c := d.conns[w.TeamID]; c != nil {
			if c.tokens == fp {
				continue
			}
			c.cancel()
		}
		c := d.connect(w, token, fp)
		if err != nil {
			c.err.Store(err.Error())
			d.log.Error("cannot poll Slack", "workspace", w.Name, "err", err)
		}
		d.conns[w.TeamID] = c
	}
	for id, c := range d.conns {
		if !want[id] {
			c.cancel()
			delete(d.conns, id)
			d.mgr.RemoveWorkspace(id)
			d.marks.forget(id) // reconnecting it later starts fresh instead of replaying the gap
		}
	}
}

// connect starts a workspace's poller. Without a token it only records why.
func (d *daemon) connect(w config.Workspace, token, fp string) *connection {
	ctx, cancel := context.WithCancel(d.ctx)
	c := &connection{tokens: fp, cancel: cancel}
	c.err.Store("")
	if token == "" {
		c.ws = &session.Workspace{TeamID: w.TeamID, Name: w.Name, UserID: w.UserID, UserName: w.UserName}
		return c
	}
	api := slack.NewClient(token)
	ws := &session.Workspace{TeamID: w.TeamID, Name: w.Name, UserID: w.UserID, UserName: w.UserName, API: api, Dir: slack.NewDirectory(api)}
	if ws.UserName == "" {
		ws.UserName = w.UserID
	}
	c.ws = ws
	d.mgr.SetWorkspace(ws)

	since := d.marks.get(w.TeamID)
	switch {
	case since.IsZero(): // first connection: nothing to catch up on
		since = time.Now()
		_ = d.marks.set(w.TeamID, since)
	default:
		since = since.Add(-catchUpMargin)
	}
	p := d.mgr.NewPoller(w.TeamID, since)
	p.ActiveSince = d.marks.agentsSince
	c.poller.Store(p)
	log := d.log.With("workspace", w.Name)
	log.Info("polling Slack", "since", since.Format(time.DateTime))
	go func() {
		t := time.NewTimer(0)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			tctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			next := p.Tick(tctx)
			cancel()
			t.Reset(next)
		}
	}()
	return c
}

// reload re-reads the config file and applies it.
func (d *daemon) reload() (*ReloadResult, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	d.mgr.SetConfig(cfg)
	d.marks.trackAgents(cfg)
	d.reconcile(cfg)
	d.log.Info("configuration reloaded", "agents", len(cfg.Agents), "workspaces", len(cfg.Workspaces))
	return &ReloadResult{Warnings: cfg.Warnings()}, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func fingerprint(s string) string {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return fmt.Sprintf("%x", h)
}
