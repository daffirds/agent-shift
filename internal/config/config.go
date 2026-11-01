// Package config defines agent-shift's YAML configuration: Slack workspaces, CLI engines
// and agents (a trigger + system prompt + engine + working directory).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/daffirds/agent-shift/internal/app"
)

const CurrentVersion = 1

// Engine names.
const (
	EngineClaude = "claude"
	EngineCodex  = "codex"
)

// Autonomy levels, mapped to each engine's permission system.
const (
	AutonomyFull     = "full"     // no prompts, no sandbox: claude bypassPermissions, codex danger-full-access
	AutonomyAuto     = "auto"     // engine's own safety reviewer decides: claude auto mode, codex auto_review
	AutonomyReadOnly = "readonly" // read-only tools, everything else denied
)

// SelfRef is an alias usable in `from`/`thread_from` lists meaning "the connected user".
const SelfRef = "me"

type Config struct {
	Version    int         `yaml:"version"`
	Daemon     Daemon      `yaml:"daemon"`
	Engines    Engines     `yaml:"engines,omitempty"`
	Workspaces []Workspace `yaml:"workspaces"`
	Agents     []Agent     `yaml:"agents"`
}

type Daemon struct {
	// MaxSessions caps how many CLI processes run at the same time. Extra work waits in a queue.
	MaxSessions int `yaml:"max_sessions,omitempty"`
	// IdleTimeout stops a session's CLI process after this long without activity.
	// The conversation is kept and resumed when the thread gets a new message.
	IdleTimeout Duration `yaml:"idle_timeout,omitempty"`
	// ThreadTTL is how long a thread keeps steering its session after the last activity.
	ThreadTTL Duration `yaml:"thread_ttl,omitempty"`
	// CatchUp: when the daemon comes back after being offline (computer off or asleep, network
	// down), it replays trigger messages and thread replies it missed, looking back at most
	// this far. "off" disables it.
	CatchUp  *Duration `yaml:"catch_up,omitempty"`
	LogLevel string    `yaml:"log_level,omitempty"`
}

// DefaultCatchUp covers a weekend with the computer off.
const DefaultCatchUp = 3 * 24 * time.Hour

// CatchUpWindow is how far back to replay missed Slack activity (0 = disabled).
func (d *Daemon) CatchUpWindow() time.Duration {
	if d.CatchUp == nil {
		return DefaultCatchUp
	}
	return max(d.CatchUp.Std(), 0)
}

type Engines struct {
	Claude Binary `yaml:"claude,omitempty"`
	Codex  Binary `yaml:"codex,omitempty"`
}

// Binary locates an engine CLI. Path is optional; the binary is looked up on PATH otherwise.
type Binary struct {
	Path string   `yaml:"path,omitempty"`
	Args []string `yaml:"args,omitempty"`
	// Env adds environment variables to every session of this engine. agent-shift strips
	// ANTHROPIC_API_KEY/ANTHROPIC_AUTH_TOKEN so your subscription is used; set them here to opt back in.
	Env map[string]string `yaml:"env,omitempty"`
}

// Workspace is a connected Slack workspace. Tokens live in the OS keychain, not here.
type Workspace struct {
	TeamID   string `yaml:"team_id"`
	Name     string `yaml:"name"`
	URL      string `yaml:"url,omitempty"`
	UserID   string `yaml:"user_id"`
	UserName string `yaml:"user_name,omitempty"`
	AppID    string `yaml:"app_id,omitempty"`
}

type Agent struct {
	Name     string `yaml:"name"`
	Disabled bool   `yaml:"disabled,omitempty"`
	Engine   string `yaml:"engine"`
	// Model and Effort (reasoning effort) choose from what the engine's CLI offers
	// (`agent-shift agents models`). Empty uses the CLI's own default.
	Model  string `yaml:"model,omitempty"`
	Effort string `yaml:"effort,omitempty"`
	// Workdir is where the CLI session runs.
	Workdir string `yaml:"workdir"`
	// Worktree runs each session in its own git worktree of Workdir, so parallel sessions don't collide.
	Worktree     bool    `yaml:"worktree,omitempty"`
	Autonomy     string  `yaml:"autonomy,omitempty"`
	SystemPrompt string  `yaml:"system_prompt,omitempty"`
	Trigger      Trigger `yaml:"trigger"`
	// ThreadFrom limits who can steer the session from the thread. Empty means anyone in the thread.
	ThreadFrom []Ref `yaml:"thread_from,omitempty"`
	// Footer is appended to every Slack message the agent sends. Default: "Assisted by Claude|Codex".
	Footer string `yaml:"footer,omitempty"`
	// ExtraArgs: claude → extra CLI flags; codex → extra `key=value` config overrides.
	ExtraArgs []string `yaml:"extra_args,omitempty"`
}

// Trigger decides which parent-level Slack messages start a new session.
type Trigger struct {
	Workspace string `yaml:"workspace"`
	// Conversations: channels, private channels, DMs or group DMs (by ID).
	Conversations []Ref `yaml:"conversations"`
	// From limits who can trigger (user IDs, bot IDs, or "me"). Empty means anyone.
	From []Ref `yaml:"from,omitempty"`
	// Contains matches if the message contains any of these strings (case-insensitive). Empty means any.
	Contains []string `yaml:"contains,omitempty"`
	// Regex additionally requires the message to match this regular expression.
	Regex string `yaml:"regex,omitempty"`
}

// Ref is a Slack ID with an optional human-readable name (the name is only for readability).
// In YAML it can be written as a plain string ("C0123") or as {id, name}.
type Ref struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name,omitempty"`
}

func (r *Ref) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		r.ID = strings.TrimSpace(n.Value)
		return nil
	}
	type plain Ref
	return n.Decode((*plain)(r))
}

func (r Ref) String() string {
	if r.Name != "" {
		return r.Name + " (" + r.ID + ")"
	}
	return r.ID
}

// Duration is a time.Duration that reads/writes as "30m", "12h" or "7d".
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) MarshalYAML() (any, error) { return FormatDuration(time.Duration(d)), nil }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

// ParseDuration is time.ParseDuration plus a "d" (day) unit; "off" means 0.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "off" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		var n float64
		if _, err := fmt.Sscanf(days, "%g", &n); err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(s)
}

func FormatDuration(d time.Duration) string {
	if d <= 0 {
		return "off"
	}
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	s := d.String() // e.g. "2h0m0s", "30m0s", "45s"
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// Defaults fills unset values.
func (c *Config) Defaults() {
	if c.Version == 0 {
		c.Version = CurrentVersion
	}
	if c.Daemon.MaxSessions <= 0 {
		c.Daemon.MaxSessions = 3
	}
	if c.Daemon.IdleTimeout <= 0 {
		c.Daemon.IdleTimeout = Duration(30 * time.Minute)
	}
	if c.Daemon.ThreadTTL <= 0 {
		c.Daemon.ThreadTTL = Duration(7 * 24 * time.Hour)
	}
	if c.Daemon.CatchUp == nil {
		d := Duration(DefaultCatchUp)
		c.Daemon.CatchUp = &d
	}
	if c.Daemon.LogLevel == "" {
		c.Daemon.LogLevel = "info"
	}
	for i := range c.Agents {
		if c.Agents[i].Autonomy == "" {
			c.Agents[i].Autonomy = AutonomyAuto
		}
	}
}

// Validate reports configuration errors that would prevent the daemon from working.
func (c *Config) Validate() error {
	var errs []error
	seen := map[string]bool{}
	for _, w := range c.Workspaces {
		if w.TeamID == "" || w.UserID == "" {
			errs = append(errs, fmt.Errorf("workspace %q: team_id and user_id are required", w.Name))
		}
	}
	for _, a := range c.Agents {
		name := a.Name
		switch {
		case name == "":
			errs = append(errs, errors.New("agent with empty name"))
			continue
		case seen[name]:
			errs = append(errs, fmt.Errorf("agent %q: duplicate name", name))
		}
		seen[name] = true
		if a.Engine != EngineClaude && a.Engine != EngineCodex {
			errs = append(errs, fmt.Errorf("agent %q: engine must be %q or %q", name, EngineClaude, EngineCodex))
		}
		if !slices.Contains([]string{AutonomyFull, AutonomyAuto, AutonomyReadOnly}, a.Autonomy) {
			errs = append(errs, fmt.Errorf("agent %q: autonomy must be full, auto or readonly", name))
		}
		if a.Workdir == "" {
			errs = append(errs, fmt.Errorf("agent %q: workdir is required", name))
		}
		if c.Workspace(a.Trigger.Workspace) == nil {
			errs = append(errs, fmt.Errorf("agent %q: trigger.workspace %q is not a connected workspace", name, a.Trigger.Workspace))
		}
		if len(a.Trigger.Conversations) == 0 {
			errs = append(errs, fmt.Errorf("agent %q: trigger.conversations is empty", name))
		}
		if a.Trigger.Regex != "" {
			if _, err := regexp.Compile(a.Trigger.Regex); err != nil {
				errs = append(errs, fmt.Errorf("agent %q: trigger.regex: %w", name, err))
			}
		}
	}
	return errors.Join(errs...)
}

// Warnings reports problems that don't stop the daemon but likely break an agent.
func (c *Config) Warnings() []string {
	var out []string
	for _, a := range c.Agents {
		if a.Disabled {
			continue
		}
		if st, err := os.Stat(a.Workdir); err != nil || !st.IsDir() {
			out = append(out, fmt.Sprintf("agent %q: workdir %s does not exist", a.Name, a.Workdir))
		}
	}
	return out
}

func (c *Config) Workspace(teamID string) *Workspace {
	for i := range c.Workspaces {
		if c.Workspaces[i].TeamID == teamID {
			return &c.Workspaces[i]
		}
	}
	return nil
}

func (c *Config) Agent(name string) *Agent {
	for i := range c.Agents {
		if c.Agents[i].Name == name {
			return &c.Agents[i]
		}
	}
	return nil
}

// UpsertWorkspace adds or replaces a workspace by team ID.
func (c *Config) UpsertWorkspace(w Workspace) {
	if cur := c.Workspace(w.TeamID); cur != nil {
		*cur = w
		return
	}
	c.Workspaces = append(c.Workspaces, w)
}

// UpsertAgent adds or replaces an agent. oldName allows renames.
func (c *Config) UpsertAgent(oldName string, a Agent) {
	if oldName == "" {
		oldName = a.Name
	}
	if cur := c.Agent(oldName); cur != nil {
		*cur = a
		return
	}
	c.Agents = append(c.Agents, a)
}

func (c *Config) RemoveAgent(name string) bool {
	n := len(c.Agents)
	c.Agents = slices.DeleteFunc(c.Agents, func(a Agent) bool { return a.Name == name })
	return len(c.Agents) != n
}

func (c *Config) RemoveWorkspace(teamID string) bool {
	n := len(c.Workspaces)
	c.Workspaces = slices.DeleteFunc(c.Workspaces, func(w Workspace) bool { return w.TeamID == teamID })
	return len(c.Workspaces) != n
}

// EngineLabel is the human name of the agent's engine ("Claude" / "Codex").
func (a *Agent) EngineLabel() string {
	if a.Engine == EngineCodex {
		return "Codex"
	}
	return "Claude"
}

func (a *Agent) FooterText() string {
	if a.Footer != "" {
		return a.Footer
	}
	return "Assisted by " + a.EngineLabel()
}

// Load reads the config file. A missing file yields an empty, defaulted config and fs.ErrNotExist.
func Load() (*Config, error) { return LoadFile(app.ConfigPath()) }

func LoadFile(path string) (*Config, error) {
	cfg := &Config{}
	data, err := os.ReadFile(path)
	if err != nil {
		cfg.Defaults()
		return cfg, err
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.Defaults()
	return cfg, nil
}

const header = `# agent-shift configuration — edit by hand or with ` + "`agent-shift setup` / `agent-shift agents`" + `.
# Apply changes to a running daemon with ` + "`agent-shift reload`" + `.
# Slack tokens are stored in your OS keychain, not in this file.
`

// Save writes the config atomically with private permissions.
func (c *Config) Save() error { return c.SaveFile(app.ConfigPath()) }

func (c *Config) SaveFile(path string) error {
	var buf bytes.Buffer
	buf.WriteString(header)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return WriteFileAtomic(path, buf.Bytes(), 0o600)
}

// WriteFileAtomic writes via a temp file + rename so readers never see partial content.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
