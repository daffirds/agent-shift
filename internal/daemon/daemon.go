// Package daemon wires everything together: one Socket Mode connection per Slack
// workspace, the session manager, the MCP tool host and a local control API, all in a
// single long-running process.
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
	"github.com/daffirds/agent-shift/internal/secret"
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

// connection is one workspace's Socket Mode link plus its ordered event worker.
type connection struct {
	ws        *session.Workspace
	sock      *slack.Socket
	tokens    string // fingerprint, to detect re-connects with new tokens
	cancel    context.CancelFunc
	connected atomic.Bool
	since     atomic.Value // time.Time of the last connect/disconnect
	lastErr   atomic.Value // string

	// Catch-up after downtime (see catchup.go).
	syncing   atomic.Bool // a replay is pending or running: the watermark must not advance
	catchMu   sync.Mutex
	catchFrom time.Time     // start of the pending replay window
	kick      chan struct{} // wakes the event worker for a replay
	catchUp   atomic.Pointer[CatchUpStatus]
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
	mgr.ResumeInterrupted()
	log.Info("agent-shift daemon running", "version", app.Version, "pid", rt.PID, "agents", len(cfg.Agents), "workspaces", len(cfg.Workspaces))
	if len(cfg.Workspaces) == 0 || len(cfg.Agents) == 0 {
		log.Warn("nothing to do yet: run `agent-shift setup` to connect Slack and create an agent")
	}

	<-ctx.Done()
	log.Info("shutting down")
	d.mu.Lock()
	for _, c := range d.conns {
		d.advance(c, time.Now()) // everything up to now was received
		c.cancel()
	}
	d.mu.Unlock()
	mgr.Shutdown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return nil
}

// reconcile starts, restarts or stops workspace connections to match the config.
func (d *daemon) reconcile(cfg *config.Config) {
	d.mu.Lock()
	defer d.mu.Unlock()
	want := map[string]bool{}
	for _, w := range cfg.Workspaces {
		want[w.TeamID] = true
		userTok, err1 := secret.Get(w.TeamID, secret.UserToken)
		appTok, err2 := secret.Get(w.TeamID, secret.AppToken)
		if err := errors.Join(err1, err2); err != nil {
			d.log.Error("missing Slack tokens; run `agent-shift slack connect`", "workspace", w.Name, "err", err)
			continue
		}
		fp := fingerprint(userTok + "|" + appTok + "|" + w.UserID)
		if c := d.conns[w.TeamID]; c != nil {
			if c.tokens == fp {
				continue
			}
			c.cancel()
		}
		d.conns[w.TeamID] = d.connect(w, userTok, appTok, fp)
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

func (d *daemon) connect(w config.Workspace, userTok, appTok, fp string) *connection {
	api := slack.NewClient(userTok)
	ws := &session.Workspace{TeamID: w.TeamID, Name: w.Name, UserID: w.UserID, UserName: w.UserName, API: api, Dir: slack.NewDirectory(api)}
	if ws.UserName == "" {
		ws.UserName = w.UserID
	}
	ws.SetAppID(w.AppID)
	d.mgr.SetWorkspace(ws)

	ctx, cancel := context.WithCancel(d.ctx)
	c := &connection{ws: ws, tokens: fp, cancel: cancel, kick: make(chan struct{}, 1)}
	log := d.log.With("workspace", w.Name)
	events := make(chan *slack.EventCallback, 1024)
	c.sock = &slack.Socket{
		App: slack.NewClient(appTok),
		Log: log,
		OnEvent: func(cb *slack.EventCallback) {
			select {
			case events <- cb:
			default:
				log.Warn("event backlog full; dropping event", "event_id", cb.EventID)
			}
		},
		OnState: func(up bool, err error) {
			if up {
				ws.SetAppID(c.sock.AppID()) // known before any event, so a catch-up recognises our own posts
				d.linkUp(c)                 // before the link counts as connected, so the watermark can't move first
			}
			was := c.connected.Swap(up)
			c.since.Store(time.Now())
			if up && !was {
				log.Info("connected to Slack (Socket Mode)", "app_id", ws.AppID())
				c.lastErr.Store("")
			}
			if err != nil {
				c.lastErr.Store(err.Error())
			}
		},
	}
	go func() { _ = c.sock.Run(ctx) }()
	go d.heartbeat(ctx, c, log)
	go func() { // one worker per workspace keeps per-thread event order
		for {
			select { // a pending catch-up goes first: it replays older messages
			case <-c.kick:
				d.runCatchUp(ctx, c, log)
				continue
			default:
			}
			select {
			case <-ctx.Done():
				return
			case <-c.kick:
				d.runCatchUp(ctx, c, log)
			case cb := <-events:
				ev, err := slack.ParseEvent(cb)
				if err != nil {
					log.Warn("cannot parse event", "err", err)
					continue
				}
				if ev != nil {
					evCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
					d.mgr.HandleEvent(evCtx, ev, cb.APIAppID)
					cancel()
				}
			}
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
