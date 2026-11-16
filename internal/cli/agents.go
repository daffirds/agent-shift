package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/daffirds/agent-shift/internal/config"
	"github.com/daffirds/agent-shift/internal/engine"
	"github.com/daffirds/agent-shift/internal/engine/claude"
	"github.com/daffirds/agent-shift/internal/engine/codex"
	"github.com/daffirds/agent-shift/internal/slack"
	"github.com/daffirds/agent-shift/internal/ui"
)

func agentsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "agents", Aliases: []string{"agent"}, Short: "List and manage agents (create and edit them in `agent-shift ui`)"}
	cmd.AddCommand(
		&cobra.Command{
			Use: "list", Short: "List agents",
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				if len(cfg.Agents) == 0 {
					ui.Warn("No agents yet. Create one in %s", ui.Code("agent-shift ui"))
					return nil
				}
				for _, a := range cfg.Agents {
					printAgent(cfg, &a)
				}
				return nil
			},
		},
		&cobra.Command{
			Use: "show <name>", Short: "Print an agent's configuration", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				a := cfg.Agent(args[0])
				if a == nil {
					return fmt.Errorf("no agent %q", args[0])
				}
				out, _ := yaml.Marshal(a)
				fmt.Print(string(out))
				return nil
			},
		},
		&cobra.Command{
			Use: "remove <name>", Aliases: []string{"rm"}, Short: "Delete an agent", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				if !cfg.RemoveAgent(args[0]) {
					return fmt.Errorf("no agent %q", args[0])
				}
				return saveConfig(cmd.Context(), cfg)
			},
		},
		toggleCmd("enable", false), toggleCmd("disable", true),
		&cobra.Command{
			Use: "models [claude|codex]", Short: "List the models and effort levels an agent can use",
			Args: cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				names := []string{config.EngineClaude, config.EngineCodex}
				if len(args) == 1 {
					if !slices.Contains(names, args[0]) {
						return fmt.Errorf("unknown engine %q: use claude or codex", args[0])
					}
					names = args
				}
				var lists []*modelList
				for _, name := range names { // ask both CLIs at once
					lists = append(lists, loadModels(cmd.Context(), cfg, &config.Agent{Engine: name}))
				}
				for _, l := range lists {
					if err := l.wait(); err != nil {
						return err
					}
					printModels(l)
				}
				ui.Line("")
				ui.Info("Pick them in %s, or set %s and %s in the agent's config.",
					ui.Code("agent-shift ui"), ui.Code("model:"), ui.Code("effort:"))
				return nil
			},
		},
	)
	return cmd
}

func toggleCmd(verb string, disabled bool) *cobra.Command {
	return &cobra.Command{
		Use: verb + " <name>", Short: strings.ToUpper(verb[:1]) + verb[1:] + " an agent", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			a := cfg.Agent(args[0])
			if a == nil {
				return fmt.Errorf("no agent %q", args[0])
			}
			a.Disabled = disabled
			return saveConfig(cmd.Context(), cfg)
		},
	}
}

func printAgent(cfg *config.Config, a *config.Agent) {
	state := "enabled"
	if a.Disabled {
		state = "disabled"
	}
	ui.Heading(a.Name + ui.Dim("  ("+state+")"))
	ws := a.Trigger.Workspace
	if w := cfg.Workspace(ws); w != nil {
		ws = w.Name
	}
	var convs, from []string
	for _, r := range a.Trigger.Conversations {
		convs = append(convs, r.String())
	}
	for _, r := range a.Trigger.From {
		from = append(from, r.String())
	}
	filter := "any message"
	if len(a.Trigger.Contains) > 0 {
		filter = "contains " + strings.Join(quoteAll(a.Trigger.Contains), " or ")
	}
	if a.Trigger.Regex != "" {
		filter += ", regex " + a.Trigger.Regex
	}
	rows := [][2]string{
		{"engine", a.EngineLabel() + modelSuffix(a) + ", autonomy " + a.Autonomy},
		{"workdir", a.Workdir + map[bool]string{true: "  (worktree per session)"}[a.Worktree]},
		{"workspace", ws},
		{"conversations", strings.Join(convs, ", ")},
		{"from", firstNonEmpty(strings.Join(from, ", "), "anyone")},
		{"filter", filter},
		{"prompt", slack.Excerpt(firstNonEmpty(a.SystemPrompt, "(none)"), 90)},
	}
	ui.KV(rows)
}

func modelSuffix(a *config.Agent) string {
	var parts []string
	if a.Model != "" {
		parts = append(parts, a.Model)
	}
	if a.Effort != "" {
		parts = append(parts, "effort "+a.Effort)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

// modelList is an engine's model list, fetched in the background (asking a CLI takes a few
// seconds).
type modelList struct {
	label string
	done  chan struct{}
	list  []engine.Model
	err   error
}

func loadModels(ctx context.Context, cfg *config.Config, a *config.Agent) *modelList {
	b := binaryOf(cfg, a.Engine)
	eng := engineFor(a.Engine)
	spec := engine.Spec{Args: b.Args, Env: b.Env, ExtraArgs: a.ExtraArgs, Workdir: a.Workdir}
	l := &modelList{label: cliName(a.Engine), done: make(chan struct{})}
	go func() {
		defer close(l.done)
		if spec.Binary, l.err = engine.Find(eng.Name(), b.Path); l.err == nil {
			l.list, l.err = eng.Models(ctx, spec)
		}
	}()
	return l
}

// wait blocks until the list is in.
func (l *modelList) wait() error {
	select {
	case <-l.done:
		return nil
	default:
	}
	return ui.Spin("Asking "+l.label+" which models you can use", func(context.Context) error {
		<-l.done
		return nil
	})
}

func printModels(l *modelList) {
	ui.Heading(l.label)
	switch {
	case errors.Is(l.err, exec.ErrNotFound):
		ui.Info("%s", ui.Dim("not installed"))
		return
	case l.err != nil:
		ui.Warn("couldn't list its models: %v", l.err)
		return
	}
	key := func(id string) string {
		if id == "" {
			return `""`
		}
		return id
	}
	w := 0
	for _, m := range l.list {
		w = max(w, len(key(m.ID)))
	}
	for _, m := range l.list {
		ui.Line("  %s%s  %s  %s", ui.Code(key(m.ID)), strings.Repeat(" ", w-len(key(m.ID))), m.Name, ui.Dim(m.Description))
		ui.Line("  %s  %s", strings.Repeat(" ", w), ui.Dim("effort: "+effortList(m)))
	}
}

func effortList(m engine.Model) string {
	if len(m.Efforts) == 0 {
		return "not adjustable"
	}
	var levels []string
	for _, e := range m.Efforts {
		levels = append(levels, e.Level)
	}
	s := strings.Join(levels, " · ")
	if m.DefaultEffort != "" {
		s += " (default " + m.DefaultEffort + ")"
	}
	return s
}

func engineFor(name string) engine.Engine {
	if name == config.EngineCodex {
		return codex.Engine{}
	}
	return claude.Engine{}
}

func cliName(eng string) string {
	if eng == config.EngineCodex {
		return "Codex"
	}
	return "Claude Code"
}

func binaryOf(cfg *config.Config, eng string) config.Binary {
	if eng == config.EngineCodex {
		return cfg.Engines.Codex
	}
	return cfg.Engines.Claude
}
