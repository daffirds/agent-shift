package webui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/daffirds/agent-shift/internal/app"
	"github.com/daffirds/agent-shift/internal/autostart"
	"github.com/daffirds/agent-shift/internal/config"
	"github.com/daffirds/agent-shift/internal/daemon"
	"github.com/daffirds/agent-shift/internal/engine"
	"github.com/daffirds/agent-shift/internal/engine/claude"
	"github.com/daffirds/agent-shift/internal/engine/codex"
	"github.com/daffirds/agent-shift/internal/link"
	"github.com/daffirds/agent-shift/internal/secret"
	"github.com/daffirds/agent-shift/internal/slack"
)

func (s *server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/workspaces", s.handleAddWorkspace)
	mux.HandleFunc("DELETE /api/workspaces/{team}", s.handleRemoveWorkspace)
	mux.HandleFunc("GET /api/workspaces/{team}/conversations", s.handleConversations)
	mux.HandleFunc("GET /api/workspaces/{team}/senders", s.handleSenders)
	mux.HandleFunc("GET /api/models/{engine}", s.handleModels)
	mux.HandleFunc("POST /api/check-dir", s.handleCheckDir)
	mux.HandleFunc("PUT /api/agents", s.handleSaveAgent)
	mux.HandleFunc("DELETE /api/agents/{name}", s.handleDeleteAgent)
	mux.HandleFunc("PUT /api/settings", s.handleSettings)
	mux.HandleFunc("POST /api/daemon/{action}", s.handleDaemon)
	mux.HandleFunc("PUT /api/autostart", s.handleAutostart)
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
}

// --- state ---

type engineState struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	LoggedIn  bool   `json:"logged_in"`
	Detail    string `json:"detail,omitempty"`
	Install   string `json:"install"`
	Login     string `json:"login"`
}

type settings struct {
	MaxSessions int    `json:"max_sessions"`
	IdleTimeout string `json:"idle_timeout"`
	ThreadTTL   string `json:"thread_ttl"`
	CatchUp     string `json:"catch_up"`
	LogLevel    string `json:"log_level"`
}

type stateResp struct {
	Slack struct {
		// CreateAppURL opens Slack's "create app from manifest" page, pre-filled.
		CreateAppURL string `json:"create_app_url"`
		Manifest     string `json:"manifest"`
	} `json:"slack"`
	Workspaces []workspaceResp `json:"workspaces"`
	Agents     []config.Agent  `json:"agents"`
	Settings   settings        `json:"settings"`
	Engines    []engineState   `json:"engines"`
	Daemon     struct {
		Running bool           `json:"running"`
		PID     int            `json:"pid,omitempty"`
		Status  *daemon.Status `json:"status,omitempty"`
		Error   string         `json:"error,omitempty"`
	} `json:"daemon"`
	Autostart struct {
		Supported bool   `json:"supported"`
		Enabled   bool   `json:"enabled"`
		Detail    string `json:"detail,omitempty"`
	} `json:"autostart"`
	Paths struct {
		Config string `json:"config"`
		Log    string `json:"log"`
	} `json:"paths"`
	Version  string   `json:"version"`
	Home     string   `json:"home"`
	Warnings []string `json:"warnings"`
}

func loadConfig() (*config.Config, error) {
	cfg, err := config.Load()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return cfg, nil
}

var (
	enginesMu   sync.Mutex
	enginesAt   time.Time
	enginesLast []engineState
)

func engines(cfg *config.Config) []engineState {
	enginesMu.Lock()
	defer enginesMu.Unlock()
	if time.Since(enginesAt) < 30*time.Second {
		return enginesLast
	}
	list := []engineState{
		{ID: config.EngineClaude, Label: "Claude Code", Install: "https://claude.com/claude-code", Login: "claude auth login"},
		{ID: config.EngineCodex, Label: "Codex", Install: "https://developers.openai.com/codex/cli", Login: "codex login"},
	}
	var wg sync.WaitGroup
	for i := range list {
		wg.Go(func() {
			e := &list[i]
			bin, err := engine.Find(e.ID, binaryOf(cfg, e.ID).Path)
			if err != nil {
				return
			}
			e.Installed, e.Version = true, engine.Version(bin)
			e.LoggedIn, e.Detail = engine.AuthStatus(e.ID, bin)
		})
	}
	wg.Wait()
	enginesLast, enginesAt = list, time.Now()
	return list
}

func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	cfg, err := loadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var st stateResp
	manifest := slack.Manifest(app.Name)
	st.Slack.Manifest, st.Slack.CreateAppURL = manifest, slack.CreateAppURL(manifest)
	st.Workspaces = []workspaceResp{}
	for _, ws := range cfg.Workspaces {
		wr := workspaceResp{Workspace: ws}
		if t, err := link.Token(ws.TeamID); err == nil {
			wr.TokenHint = secret.Hint(t)
		} else {
			wr.TokenError = err.Error()
		}
		if ws.AppID != "" {
			wr.AppURL = slack.AppSettingsURL(ws.AppID, "oauth")
		}
		st.Workspaces = append(st.Workspaces, wr)
	}
	st.Agents = nonNil(cfg.Agents)
	st.Settings = settings{
		MaxSessions: cfg.Daemon.MaxSessions,
		IdleTimeout: config.FormatDuration(cfg.Daemon.IdleTimeout.Std()),
		ThreadTTL:   config.FormatDuration(cfg.Daemon.ThreadTTL.Std()),
		CatchUp:     config.FormatDuration(cfg.Daemon.CatchUpWindow()),
		LogLevel:    cfg.Daemon.LogLevel,
	}
	st.Engines = engines(cfg)
	if c, err := daemon.Connect(); err == nil {
		st.Daemon.Running, st.Daemon.PID = true, c.Runtime().PID
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		if ds, err := c.Status(ctx); err == nil {
			st.Daemon.Status = ds
		} else {
			st.Daemon.Error = err.Error()
		}
		cancel()
	}
	as := autostart.Get()
	st.Autostart.Supported = runtime.GOOS == "darwin" || runtime.GOOS == "windows"
	st.Autostart.Enabled, st.Autostart.Detail = as.Installed, as.Detail
	st.Paths.Config, st.Paths.Log = app.ConfigPath(), app.LogPath()
	st.Version = app.Version
	st.Home, _ = os.UserHomeDir()
	st.Warnings = nonNil(cfg.Warnings())
	writeJSON(w, http.StatusOK, st)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// save validates and writes the config, then asks a running daemon to apply it.
func save(ctx context.Context, cfg *config.Config) (map[string]any, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := app.EnsureHome(); err != nil {
		return nil, err
	}
	if err := cfg.Save(); err != nil {
		return nil, err
	}
	out := map[string]any{"ok": true, "reloaded": false, "warnings": nonNil(cfg.Warnings())}
	return out, reloadDaemon(ctx, out)
}

func reloadDaemon(ctx context.Context, out map[string]any) error {
	c, err := daemon.Connect()
	if err != nil {
		return nil
	}
	res, err := c.Reload(ctx)
	if err != nil {
		return fmt.Errorf("saved, but the daemon rejected the new config: %w", err)
	}
	out["reloaded"] = true
	out["warnings"] = nonNil(res.Warnings)
	return nil
}

// --- workspaces ---

type workspaceResp struct {
	config.Workspace
	TokenHint  string `json:"token_hint,omitempty"`
	TokenError string `json:"token_error,omitempty"`
	AppURL     string `json:"app_url,omitempty"`
}

// handleAddWorkspace takes a User OAuth Token, finds out whose it is, stores it in the
// keychain and adds (or updates) the workspace.
func (s *server) handleAddWorkspace(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	token := strings.TrimSpace(in.Token)
	cfg, err := loadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	id, err := link.Identify(ctx, token)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if old := cfg.Workspace(id.Workspace.TeamID); old != nil {
		if old.UserID != id.Workspace.UserID {
			writeErr(w, http.StatusConflict, fmt.Errorf("%s is already connected as another user; remove it first", old.Name))
			return
		}
		if id.Workspace.AppID == "" {
			id.Workspace.AppID = old.AppID
		}
	}
	if err := secret.Set(secret.SlackToken(id.Workspace.TeamID), token); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("store the token: %w", err))
		return
	}
	cfg.UpsertWorkspace(id.Workspace)
	out, err := save(r.Context(), cfg)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out["workspace"] = id.Workspace
	out["missing_scopes"] = id.MissingScopes
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleRemoveWorkspace(w http.ResponseWriter, r *http.Request) {
	cfg, err := loadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	team := r.PathValue("team")
	var users []string
	for _, a := range cfg.Agents {
		if a.Trigger.Workspace == team {
			users = append(users, a.Name)
		}
	}
	if len(users) > 0 {
		writeErr(w, http.StatusConflict, fmt.Errorf("agents still use this workspace: %s", strings.Join(users, ", ")))
		return
	}
	if !cfg.RemoveWorkspace(team) {
		writeErr(w, http.StatusNotFound, errors.New("no such workspace"))
		return
	}
	out, err := save(r.Context(), cfg)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := secret.DeleteWorkspace(team); err != nil {
		out["warning"] = "could not delete the token from the keychain: " + err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

func directory(cfg *config.Config, team string) (*slack.Directory, *config.Workspace, error) {
	ws := cfg.Workspace(team)
	if ws == nil {
		return nil, nil, errors.New("no such workspace")
	}
	api, err := link.Slack(ws)
	if err != nil {
		return nil, nil, err
	}
	return slack.NewDirectory(api), ws, nil
}

type conversationResp struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Name  string `json:"name"` // what goes into the config's `name`
	Kind  string `json:"kind"` // channel, private, dm, self, group
}

func (s *server) handleConversations(w http.ResponseWriter, r *http.Request) {
	cfg, err := loadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	dir, ws, err := directory(cfg, r.PathValue("team"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	convs, err := dir.MyConversations(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	names := map[string]string{}
	if users, err := dir.AllUsers(ctx); err == nil {
		for _, u := range users {
			names[u.ID] = u.Display()
		}
	}
	rank := map[string]int{"self": 0, "channel": 1, "private": 2, "dm": 3, "group": 4}
	out := []conversationResp{}
	for _, c := range convs {
		var it conversationResp
		switch {
		case c.IsIM && c.User == ws.UserID:
			it = conversationResp{Label: firstNonEmpty(names[c.User], "you") + " (notes to self)", Name: "notes to self", Kind: "self"}
		case c.IsIM:
			n := firstNonEmpty(names[c.User], c.User)
			it = conversationResp{Label: n, Name: "DM with " + n, Kind: "dm"}
		case c.IsMPIM:
			n := strings.ReplaceAll(strings.TrimPrefix(c.Name, "mpdm-"), "--", ", ")
			n = strings.TrimSuffix(n, "-1")
			it = conversationResp{Label: n, Name: n, Kind: "group"}
		case c.IsPrivate:
			it = conversationResp{Label: c.Name, Name: c.Name, Kind: "private"}
		default:
			it = conversationResp{Label: c.Name, Name: c.Name, Kind: "channel"}
		}
		it.ID = c.ID
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].Kind] != rank[out[j].Kind] {
			return rank[out[i].Kind] < rank[out[j].Kind]
		}
		return strings.ToLower(out[i].Label) < strings.ToLower(out[j].Label)
	})
	writeJSON(w, http.StatusOK, out)
}

type senderResp struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"` // person, app, workflow
}

// handleSenders lists who posted recently in the given conversations (people, bots, workflows).
func (s *server) handleSenders(w http.ResponseWriter, r *http.Request) {
	cfg, err := loadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	dir, ws, err := directory(cfg, r.PathValue("team"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	found := map[string]senderResp{}
	for _, c := range strings.Split(r.URL.Query().Get("c"), ",") {
		if c = strings.TrimSpace(c); c == "" {
			continue
		}
		page, err := dir.API().History(ctx, c, slack.HistoryOptions{Limit: 200})
		if err != nil {
			continue
		}
		for i := range page.Messages {
			m := &page.Messages[i]
			id := m.SenderID()
			if id == "" || id == ws.UserID || found[id].ID != "" {
				continue
			}
			kind := "person"
			switch {
			case m.User == "":
				kind = "workflow"
			case m.BotID != "" || m.BotProfile != nil:
				kind = "app"
			}
			found[id] = senderResp{ID: id, Name: dir.SenderName(ctx, m), Kind: kind}
		}
	}
	out := []senderResp{}
	for _, v := range found {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	writeJSON(w, http.StatusOK, out)
}

// --- engines and models ---

type modelCache struct {
	at   time.Time
	list []modelResp
	err  string
}

type effortResp struct {
	Level       string `json:"level"`
	Description string `json:"description,omitempty"`
}

type modelResp struct {
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	Description   string       `json:"description,omitempty"`
	Efforts       []effortResp `json:"efforts"`
	DefaultEffort string       `json:"default_effort,omitempty"`
}

func (s *server) handleModels(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("engine")
	if name != config.EngineClaude && name != config.EngineCodex {
		writeErr(w, http.StatusBadRequest, errors.New("unknown engine"))
		return
	}
	s.mu.Lock()
	c, ok := s.models[name]
	s.mu.Unlock()
	if !ok || time.Since(c.at) > 10*time.Minute {
		cfg, err := loadConfig()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		b := binaryOf(cfg, name)
		eng := engineFor(name)
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		c = modelCache{at: time.Now(), list: []modelResp{}}
		bin, err := engine.Find(name, b.Path)
		var list []engine.Model
		if err == nil {
			home, _ := os.UserHomeDir()
			list, err = eng.Models(ctx, engine.Spec{Binary: bin, Args: b.Args, Env: b.Env, Workdir: home})
		}
		if err != nil {
			c.err = err.Error()
		}
		for _, m := range list {
			mr := modelResp{ID: m.ID, Name: m.Name, Description: m.Description, DefaultEffort: m.DefaultEffort, Efforts: []effortResp{}}
			for _, e := range m.Efforts {
				mr.Efforts = append(mr.Efforts, effortResp{Level: e.Level, Description: e.Description})
			}
			c.list = append(c.list, mr)
		}
		if c.err == "" || ctx.Err() == nil { // don't cache a cancelled request
			s.mu.Lock()
			s.models[name] = c
			s.mu.Unlock()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": c.list, "error": c.err})
}

func engineFor(name string) engine.Engine {
	if name == config.EngineCodex {
		return codex.Engine{}
	}
	return claude.Engine{}
}

func binaryOf(cfg *config.Config, eng string) config.Binary {
	if eng == config.EngineCodex {
		return cfg.Engines.Codex
	}
	return cfg.Engines.Claude
}

func (s *server) handleCheckDir(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	p := normalizeDir(in.Path)
	st, err := os.Stat(p)
	writeJSON(w, http.StatusOK, map[string]any{"path": p, "exists": err == nil && st.IsDir(), "git": isGitRepo(p)})
}

func normalizeDir(p string) string {
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return p
}

func isGitRepo(dir string) bool {
	if dir == "" {
		return false
	}
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return true
		}
		if filepath.Dir(d) == d {
			return false
		}
	}
}

// --- agents ---

var agentNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)

func (s *server) handleSaveAgent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OldName string       `json:"old_name"`
		Agent   config.Agent `json:"agent"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	cfg, err := loadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	a := in.Agent
	a.Name = strings.TrimSpace(a.Name)
	a.Workdir = normalizeDir(a.Workdir)
	a.SystemPrompt = strings.TrimSpace(a.SystemPrompt)
	a.Model, a.Effort = strings.TrimSpace(a.Model), strings.TrimSpace(a.Effort)
	a.Trigger.Regex = strings.TrimSpace(a.Trigger.Regex)
	a.Trigger.Contains = slices.DeleteFunc(trimAll(a.Trigger.Contains), func(s string) bool { return s == "" })
	var problems []string
	if !agentNamePattern.MatchString(a.Name) {
		problems = append(problems, "name: letters, digits, dot, dash or underscore (max 40)")
	}
	if other := cfg.Agent(a.Name); other != nil && in.OldName != a.Name {
		problems = append(problems, "name: an agent with this name already exists")
	}
	if in.OldName != "" && cfg.Agent(in.OldName) == nil {
		problems = append(problems, "the agent you were editing no longer exists")
	}
	if st, err := os.Stat(a.Workdir); err != nil || !st.IsDir() {
		problems = append(problems, "working directory does not exist")
	}
	if a.Worktree && !isGitRepo(a.Workdir) {
		a.Worktree = false
	}
	if a.Engine == config.EngineClaude || a.Engine == config.EngineCodex {
		if _, err := engine.Find(a.Engine, binaryOf(cfg, a.Engine).Path); err != nil {
			problems = append(problems, a.Engine+" CLI not found on this machine")
		}
	}
	if len(problems) > 0 {
		writeErr(w, http.StatusBadRequest, errors.New(strings.Join(problems, "; ")))
		return
	}
	cfg.UpsertAgent(in.OldName, a)
	cfg.Defaults()
	out, err := save(r.Context(), cfg)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func trimAll(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

func (s *server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	cfg, err := loadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !cfg.RemoveAgent(r.PathValue("name")) {
		writeErr(w, http.StatusNotFound, errors.New("no such agent"))
		return
	}
	out, err := save(r.Context(), cfg)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// --- daemon ---

func (s *server) handleSettings(w http.ResponseWriter, r *http.Request) {
	var in settings
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	cfg, err := loadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var problems []string
	parse := func(label, v string, allowOff bool) config.Duration {
		d, err := config.ParseDuration(v)
		if err != nil || (d <= 0 && !allowOff) {
			problems = append(problems, label+`: use a duration like "30m", "12h" or "7d"`)
		}
		return config.Duration(d)
	}
	if in.MaxSessions < 1 || in.MaxSessions > 50 {
		problems = append(problems, "max sessions: 1–50")
	}
	idle := parse("idle timeout", in.IdleTimeout, false)
	ttl := parse("thread TTL", in.ThreadTTL, false)
	catch := parse("catch-up", in.CatchUp, true)
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, in.LogLevel) {
		problems = append(problems, "log level: debug, info, warn or error")
	}
	if len(problems) > 0 {
		writeErr(w, http.StatusBadRequest, errors.New(strings.Join(problems, "; ")))
		return
	}
	cfg.Daemon.MaxSessions, cfg.Daemon.IdleTimeout, cfg.Daemon.ThreadTTL = in.MaxSessions, idle, ttl
	cfg.Daemon.CatchUp = &catch
	cfg.Daemon.LogLevel = in.LogLevel
	out, err := save(r.Context(), cfg)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleDaemon(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	var err error
	switch r.PathValue("action") {
	case "start":
		_, _, err = daemon.Start(ctx)
	case "stop":
		_, err = daemon.Stop(ctx)
	case "restart":
		if _, err = daemon.Stop(ctx); err == nil {
			_, _, err = daemon.Start(ctx)
		}
	default:
		writeErr(w, http.StatusNotFound, errors.New("unknown action"))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleAutostart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if !in.Enabled {
		if err := autostart.Uninstall(); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	exe, err := os.Executable()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	_, running := daemon.Connect()
	isRunning := running == nil
	if isRunning && runtime.GOOS == "darwin" { // launchd owns the daemon from now on
		if _, err := daemon.Stop(ctx); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	if _, err := autostart.Install(exe, os.Getenv("PATH")); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if runtime.GOOS == "darwin" || !isRunning {
		if _, _, err := daemon.Start(ctx); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) handleSessions(w http.ResponseWriter, r *http.Request) {
	c, err := daemon.Connect()
	if err != nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	list, err := c.Sessions(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(list))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
