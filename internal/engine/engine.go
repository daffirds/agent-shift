// Package engine drives local coding-agent CLIs (Claude Code, Codex) as long-lived,
// steerable sessions. The CLIs use their own logins, so subscriptions work as-is.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/daffirds/agent-shift/internal/proc"
)

// Spec describes how to start or resume a session.
type Spec struct {
	Binary       string            // absolute path of the CLI
	Args         []string          // global extra args (engines.<name>.args)
	Env          map[string]string // extra environment (engines.<name>.env)
	ExtraArgs    []string          // per-agent extras (claude: flags; codex: key=value overrides)
	Workdir      string
	Instructions string // appended to the engine's own system prompt
	Model        string // "" = the CLI's default
	Effort       string // reasoning effort level; "" = the model's default
	Autonomy     string // config.Autonomy*
	MCP          MCPServer
	AddDirs      []string // extra directories the agent may read/write (claude --add-dir)
	ResumeID     string   // engine-native session id to resume; empty starts fresh
	StateDir     string   // private per-session directory for prompt/config files
	Log          *slog.Logger
	Transcript   *TranscriptLog // receives raw protocol lines (optional)
}

// MCPServer is the agent-shift Slack tool server the session connects to.
type MCPServer struct {
	Name string
	URL  string
}

type EventKind int

const (
	TurnStarted EventKind = iota
	TurnCompleted
	AssistantText
	ToolCall
	Notice // warnings worth logging (MCP failures, retries…)
)

type Event struct {
	Kind  EventKind
	Text  string
	Error string // TurnCompleted: non-empty when the turn failed
}

// Session is a running CLI process bound to one conversation.
type Session interface {
	// ID is the engine-native conversation id used to resume later.
	ID() string
	// Send delivers a user message. If a turn is running it is steered into it,
	// otherwise a new turn starts.
	Send(ctx context.Context, text string) error
	// Interrupt stops the current turn, keeping the session.
	Interrupt(ctx context.Context) error
	Events() <-chan Event
	// Done is closed when the process exits; Err tells why.
	Done() <-chan struct{}
	Err() error
	// Close shuts the process down (gracefully, then forcefully).
	Close() error
}

type Engine interface {
	Name() string
	Start(ctx context.Context, spec Spec) (Session, error)
	// Models lists the models the installed CLI offers to its logged-in account, without
	// sending a prompt. Only Binary, Args, Env, ExtraArgs and Workdir of spec are used.
	Models(ctx context.Context, spec Spec) ([]Model, error)
}

// Model is one entry of an engine's model picker. The first entry has ID "": the CLI's
// own default, whatever its settings say.
type Model struct {
	ID            string
	Name          string
	Description   string
	Efforts       []Effort // reasoning effort levels the model accepts; none if it has no setting
	DefaultEffort string   // "" when the CLI doesn't say
}

type Effort struct {
	Level       string
	Description string
}

var ErrClosed = errors.New("session closed")

// Find locates an engine CLI: the configured path, then PATH, then common install locations
// (needed when the daemon runs from launchd / Task Scheduler with a minimal PATH).
func Find(name, configured string) (string, error) {
	if configured != "" {
		if _, err := os.Stat(configured); err != nil {
			return "", err
		}
		return configured, nil
	}
	if p, err := exec.LookPath(name); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs, nil
		}
		return p, nil
	}
	home, _ := os.UserHomeDir()
	var candidates []string
	if runtime.GOOS == "windows" {
		local := os.Getenv("LOCALAPPDATA")
		appdata := os.Getenv("APPDATA")
		candidates = []string{
			filepath.Join(home, ".local", "bin", name+".exe"),
			filepath.Join(local, "Programs", "OpenAI", "Codex", "bin", name+".exe"),
			filepath.Join(appdata, "npm", name+".cmd"),
			filepath.Join(home, ".bun", "bin", name+".exe"),
		}
	} else {
		candidates = []string{
			filepath.Join(home, ".local", "bin", name),
			filepath.Join(home, ".claude", "local", name),
			"/opt/homebrew/bin/" + name,
			"/usr/local/bin/" + name,
			filepath.Join(home, ".npm-global", "bin", name),
			filepath.Join(home, ".bun", "bin", name),
			filepath.Join(home, ".volta", "bin", name),
		}
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", exec.ErrNotFound
}

// Version runs `<bin> --version` (best-effort, for diagnostics).
func Version(bin string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	proc.HideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	// "2.1.316 (Claude Code)" / "codex-cli 0.166.0" → the version number.
	for _, f := range strings.Fields(string(out)) {
		if f != "" && f[0] >= '0' && f[0] <= '9' {
			return f
		}
	}
	return strings.TrimSpace(string(out))
}

// AuthStatus reports whether the CLI is logged in, using `claude auth status` /
// `codex login status`. detail describes the login (e.g. "claude.ai, max plan").
func AuthStatus(name, bin string) (ok bool, detail string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := []string{"auth", "status"}
	if name == "codex" {
		args = []string{"login", "status"}
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	proc.HideWindow(cmd)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if name == "codex" {
		return err == nil && strings.Contains(strings.ToLower(text), "logged in"), text
	}
	var st struct {
		LoggedIn         bool   `json:"loggedIn"`
		AuthMethod       string `json:"authMethod"`
		SubscriptionType string `json:"subscriptionType"`
		Email            string `json:"email"`
	}
	if json.Unmarshal(out, &st) != nil {
		return err == nil, text
	}
	detail = st.AuthMethod
	if st.SubscriptionType != "" {
		detail += ", " + st.SubscriptionType + " plan"
	}
	if st.Email != "" {
		detail += ", " + st.Email
	}
	return st.LoggedIn, detail
}

// TranscriptLog appends raw protocol lines to a JSONL file for debugging (`agent-shift sessions logs`).
type TranscriptLog struct {
	mu sync.Mutex
	w  io.Writer
}

func NewTranscriptLog(w io.Writer) *TranscriptLog { return &TranscriptLog{w: w} }

// Write records one protocol line; dir is "in" (from the CLI) or "out" (to the CLI).
func (t *TranscriptLog) Write(dir string, line []byte) {
	if t == nil || t.w == nil {
		return
	}
	rec := struct {
		T   string          `json:"t"`
		Dir string          `json:"dir"`
		Msg json.RawMessage `json:"msg"`
	}{time.Now().Format(time.RFC3339Nano), dir, line}
	if !json.Valid(line) {
		rec.Msg, _ = json.Marshal(string(line))
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = t.w.Write(append(b, '\n'))
}

// StderrTail keeps the last few KB of a process's stderr for error messages.
type StderrTail struct {
	mu  sync.Mutex
	buf []byte
}

func (r *StderrTail) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > 8192 {
		r.buf = r.buf[len(r.buf)-8192:]
	}
	return len(p), nil
}

// Suffix returns ": <last stderr lines>" for appending to an error, or "".
func (r *StderrTail) Suffix() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := strings.TrimSpace(string(r.buf))
	if s == "" {
		return ""
	}
	if len(s) > 800 {
		s = "…" + s[len(s)-800:]
	}
	return ": " + s
}
