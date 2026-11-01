package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/daffirds/agent-shift/internal/config"
	"github.com/daffirds/agent-shift/internal/engine"
	"github.com/daffirds/agent-shift/internal/engine/claude"
	"github.com/daffirds/agent-shift/internal/engine/codex"
	"github.com/daffirds/agent-shift/internal/secret"
	"github.com/daffirds/agent-shift/internal/slack"
	"github.com/daffirds/agent-shift/internal/ui"
)

func agentsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "agents", Aliases: []string{"agent"}, Short: "Create and manage agents (trigger + prompt + engine + workdir)"}
	cmd.AddCommand(
		&cobra.Command{
			Use: "list", Short: "List agents",
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				if len(cfg.Agents) == 0 {
					ui.Warn("No agents yet. Run %s", ui.Code("agent-shift agents add"))
					return nil
				}
				for _, a := range cfg.Agents {
					printAgent(cfg, &a)
				}
				return nil
			},
		},
		&cobra.Command{
			Use: "add", Short: "Create an agent interactively",
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				a, err := agentForm(cmd.Context(), cfg, nil)
				if err != nil {
					return err
				}
				cfg.UpsertAgent("", *a)
				return saveConfig(cmd.Context(), cfg)
			},
		},
		&cobra.Command{
			Use: "edit <name>", Short: "Edit an agent interactively", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				cur := cfg.Agent(args[0])
				if cur == nil {
					return fmt.Errorf("no agent %q", args[0])
				}
				old := *cur
				a, err := agentForm(cmd.Context(), cfg, &old)
				if err != nil {
					return err
				}
				cfg.UpsertAgent(old.Name, *a)
				return saveConfig(cmd.Context(), cfg)
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
				ui.Info("Pick them with %s, or set %s and %s in the agent's config.",
					ui.Code("agent-shift agents edit <name>"), ui.Code("model:"), ui.Code("effort:"))
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

var agentNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)

// agentForm collects an agent definition. existing pre-fills the form when editing.
func agentForm(ctx context.Context, cfg *config.Config, existing *config.Agent) (*config.Agent, error) {
	if len(cfg.Workspaces) == 0 {
		return nil, errors.New("connect Slack first: agent-shift slack connect")
	}
	a := config.Agent{Engine: config.EngineClaude, Autonomy: config.AutonomyAuto}
	if existing != nil {
		a = *existing
	} else if wd, err := os.Getwd(); err == nil {
		a.Workdir = wd
	}

	// --- Basics ---
	engineOpts := []huh.Option[string]{}
	for _, e := range []struct{ id, label string }{{config.EngineClaude, "Claude Code"}, {config.EngineCodex, "Codex"}} {
		label := e.label
		if _, err := engine.Find(e.id, binaryOf(cfg, e.id).Path); err != nil {
			label += ui.Dim("  (not installed)")
		}
		engineOpts = append(engineOpts, huh.NewOption(label, e.id))
	}
	validateName := func(s string) error {
		if !agentNamePattern.MatchString(s) {
			return errors.New("letters, digits, dot, dash or underscore (max 40)")
		}
		if other := cfg.Agent(s); other != nil && (existing == nil || existing.Name != s) {
			return errors.New("an agent with this name already exists")
		}
		return nil
	}
	validateEngine := func(s string) error {
		if _, err := engine.Find(s, binaryOf(cfg, s).Path); err != nil {
			return fmt.Errorf("%s CLI not found on this machine", s)
		}
		return nil
	}
	ui.Heading("Agent")
	if err := ui.Form(huh.NewGroup(
		huh.NewInput().Title("Name").Description("Short id, e.g. ticket-triage").Value(&a.Name).
			Validate(keepDefault(a.Name, validateName)),
		huh.NewSelect[string]().Title("Engine").Options(engineOpts...).Value(&a.Engine).Validate(validateEngine),
		huh.NewInput().Title("Working directory").Description("Where the CLI session runs (usually a repo).").
			Value(&a.Workdir).Validate(keepDefault(a.Workdir, validateDir)),
	)); err != nil {
		return nil, err
	}
	a.Name = strings.TrimSpace(a.Name)
	a.Workdir = expandHome(strings.TrimSpace(a.Workdir))
	if abs, err := filepath.Abs(a.Workdir); err == nil {
		a.Workdir = abs
	}
	// Line-by-line prompts can end early (closed input) and hand back a rejected value: re-check.
	if err := errors.Join(validateName(a.Name), validateEngine(a.Engine), validateDir(a.Workdir)); err != nil {
		return nil, err
	}
	if existing != nil && a.Engine != existing.Engine {
		a.Model, a.Effort = "", "" // the other CLI's names mean nothing here
	}
	models := loadModels(ctx, cfg, &a) // usually ready by the time the model question comes

	// --- Trigger ---
	ws := &cfg.Workspaces[0]
	if len(cfg.Workspaces) > 1 {
		teamID := a.Trigger.Workspace
		var opts []huh.Option[string]
		for _, w := range cfg.Workspaces {
			opts = append(opts, huh.NewOption(w.Name, w.TeamID))
		}
		if err := ui.Form(huh.NewGroup(huh.NewSelect[string]().Title("Slack workspace").Options(opts...).Value(&teamID))); err != nil {
			return nil, err
		}
		ws = cfg.Workspace(teamID)
	}
	if ws.TeamID != a.Trigger.Workspace {
		a.Trigger = config.Trigger{Workspace: ws.TeamID}
	}
	tok, err := secret.Get(ws.TeamID, secret.UserToken)
	if err != nil {
		return nil, fmt.Errorf("no token for %s; run agent-shift slack connect", ws.Name)
	}
	dir := slack.NewDirectory(slack.NewClient(tok))

	ui.Heading("Trigger")
	convs, err := pickConversations(ctx, dir, ws, a.Trigger.Conversations)
	if err != nil {
		return nil, err
	}
	a.Trigger.Conversations = convs

	from, err := pickSenders(ctx, dir, ws, convs, a.Trigger.From)
	if err != nil {
		return nil, err
	}
	a.Trigger.From = from

	contains := strings.Join(a.Trigger.Contains, ", ")
	if err := ui.Form(huh.NewGroup(
		huh.NewInput().Title("Only messages containing…").
			Description("Comma-separated, case-insensitive; any of them matches. Leave empty for every message.").
			Placeholder("e.g. New ticket, [bug]").Value(&contains),
		huh.NewInput().Title("…and matching this regex (optional)").Placeholder(`e.g. (?i)priority:\s*(high|urgent)`).
			Value(&a.Trigger.Regex).Validate(func(s string) error {
			if s == "" {
				return nil
			}
			_, err := regexp.Compile(s)
			return err
		}),
	)); err != nil {
		return nil, err
	}
	a.Trigger.Contains = splitList(contains)

	// --- Behaviour ---
	ui.Heading("Behaviour")
	steer := "anyone"
	switch {
	case len(a.ThreadFrom) == 1 && a.ThreadFrom[0].ID == config.SelfRef:
		steer = "me"
	case len(a.ThreadFrom) > 0:
		steer = "trigger"
	}
	fields := []huh.Field{
		huh.NewText().Title("System prompt").
			Description("What should the agent do with these messages? (Ctrl+E opens your editor)").
			Placeholder("e.g. You triage support tickets for the payments repo. Reproduce the bug, open a draft PR with a fix, and reply in the thread with the PR link.").
			Lines(8).Value(&a.SystemPrompt),
		huh.NewSelect[string]().Title("Autonomy").Options(
			huh.NewOption("Auto — the engine's safety reviewer approves actions (recommended)", config.AutonomyAuto),
			huh.NewOption("Full — no approvals, no sandbox (needed for unattended PRs, deploys…)", config.AutonomyFull),
			huh.NewOption("Read-only — read and answer, never change anything", config.AutonomyReadOnly),
		).Value(&a.Autonomy),
		huh.NewSelect[string]().Title("Who can steer the session from the thread?").Options(
			huh.NewOption("Anyone who replies or reacts in the thread", "anyone"),
			huh.NewOption("Only me", "me"),
			huh.NewOption("Me and the trigger senders", "trigger"),
		).Value(&steer),
	}
	if isGitRepo(a.Workdir) {
		fields = append(fields, huh.NewConfirm().Title("Give each session its own git worktree?").
			Description("Lets several sessions work on the same repo at once without clashing.").Value(&a.Worktree))
	} else {
		a.Worktree = false
	}
	if err := ui.Form(huh.NewGroup(fields...)); err != nil {
		return nil, err
	}
	a.SystemPrompt = strings.TrimSpace(a.SystemPrompt)
	switch steer {
	case "me":
		a.ThreadFrom = []config.Ref{{ID: config.SelfRef}}
	case "trigger":
		a.ThreadFrom = append([]config.Ref{{ID: config.SelfRef}}, a.Trigger.From...)
	default:
		a.ThreadFrom = nil
	}

	// --- Model ---
	ui.Heading("Model")
	if err := models.wait(); err != nil {
		return nil, err
	}
	if err := pickModel(&a, models.list, models.err); err != nil {
		return nil, err
	}

	printAgent(cfg, &a)
	// Non-interactive input must say "y" explicitly; running out of answers never saves.
	ok, err := ui.Confirm("Save this agent?", "", !ui.Accessible())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ui.ErrAborted
	}
	return &a, nil
}

// modelList is an engine's model list, fetched in the background while the user answers
// other questions (asking a CLI takes a few seconds).
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

// wait blocks until the list is in, showing a spinner if that takes a moment.
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

// pickModel chooses the agent's model and reasoning effort from what its CLI offers, or asks
// for them as text when the CLI couldn't list its models.
func pickModel(a *config.Agent, list []engine.Model, listErr error) error {
	if len(list) == 0 {
		if listErr != nil {
			ui.Warn("Couldn't get the model list from %s: %v", cliName(a.Engine), listErr)
		}
		if err := ui.Form(huh.NewGroup(
			huh.NewInput().Title("Model (optional)").Description("Leave empty for the CLI's default.").Value(&a.Model),
			huh.NewInput().Title("Reasoning effort (optional)").
				Description("e.g. low, medium or high. Leave empty for the model's default.").Value(&a.Effort),
		)); err != nil {
			return err
		}
		a.Model, a.Effort = strings.TrimSpace(a.Model), strings.TrimSpace(a.Effort)
		return nil
	}

	const other = "\x00other"
	choice := other
	if slices.ContainsFunc(list, func(m engine.Model) bool { return m.ID == a.Model }) {
		choice = a.Model
	}
	var opts []huh.Option[string]
	for _, m := range list {
		opts = append(opts, huh.NewOption(m.Name+ui.Dim("  "+m.Description), m.ID))
	}
	opts = append(opts, huh.NewOption("Other…"+ui.Dim("  type a model name"), other))
	if err := ui.Form(huh.NewGroup(huh.NewSelect[string]().Title("Model").
		Description("As offered by your " + cliName(a.Engine) + " login.").Options(opts...).Value(&choice))); err != nil {
		return err
	}
	if choice == other {
		if err := ui.Form(huh.NewGroup(huh.NewInput().Title("Model name").
			Description("Any name the CLI accepts, e.g. a full model id.").Value(&a.Model))); err != nil {
			return err
		}
		a.Model = strings.TrimSpace(a.Model)
	} else {
		a.Model = choice
	}

	efforts, def := allEfforts(list), "" // a model the CLI didn't list: offer every level seen
	if i := slices.IndexFunc(list, func(m engine.Model) bool { return m.ID == a.Model }); i >= 0 {
		efforts, def = list[i].Efforts, list[i].DefaultEffort
	}
	if !slices.ContainsFunc(efforts, func(e engine.Effort) bool { return e.Level == a.Effort }) {
		a.Effort = ""
	}
	if len(efforts) == 0 {
		return nil
	}
	label := "Default"
	if def != "" {
		label += ui.Dim(" (" + def + ")")
	}
	opts = []huh.Option[string]{huh.NewOption(label, "")}
	for _, e := range efforts {
		label := e.Level
		if e.Description != "" {
			label += ui.Dim("  " + e.Description)
		}
		opts = append(opts, huh.NewOption(label, e.Level))
	}
	return ui.Form(huh.NewGroup(huh.NewSelect[string]().Title("Reasoning effort").
		Description("Higher levels think longer, which is slower and uses more of your plan.").
		Options(opts...).Value(&a.Effort)))
}

// allEfforts is every effort level any listed model accepts.
func allEfforts(list []engine.Model) []engine.Effort {
	var out []engine.Effort
	for _, m := range list {
		for _, e := range m.Efforts {
			if !slices.ContainsFunc(out, func(x engine.Effort) bool { return x.Level == e.Level }) {
				out = append(out, e)
			}
		}
	}
	return out
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

// pickConversations lets the user choose channels / DMs / group DMs they are a member of.
func pickConversations(ctx context.Context, dir *slack.Directory, ws *config.Workspace, current []config.Ref) ([]config.Ref, error) {
	var convs []slack.Conversation
	var users map[string]string
	err := ui.Spin("Loading your channels and DMs", func(ctx context.Context) error {
		var err error
		if convs, err = dir.MyConversations(ctx); err != nil {
			return err
		}
		all, err := dir.AllUsers(ctx)
		users = map[string]string{}
		for _, u := range all {
			users[u.ID] = u.Display()
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	type item struct {
		label, id string
		rank      int
	}
	var items []item
	for _, c := range convs {
		switch {
		case c.IsIM && c.User == ws.UserID:
			items = append(items, item{"✎ " + users[c.User] + " (you — notes to self)", c.ID, 0})
		case c.IsIM:
			name := firstNonEmpty(users[c.User], c.User)
			items = append(items, item{"@ " + name + "  (DM)", c.ID, 3})
		case c.IsMPIM:
			items = append(items, item{"⧉ " + strings.ReplaceAll(strings.TrimPrefix(c.Name, "mpdm-"), "--", ", ") + "  (group DM)", c.ID, 4})
		case c.IsPrivate:
			items = append(items, item{"🔒 " + c.Name, c.ID, 2})
		default:
			items = append(items, item{"# " + c.Name, c.ID, 1})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].rank != items[j].rank {
			return items[i].rank < items[j].rank
		}
		return strings.ToLower(items[i].label) < strings.ToLower(items[j].label)
	})
	selected := map[string]bool{}
	for _, r := range current {
		selected[r.ID] = true
	}
	var opts []huh.Option[string]
	labels := map[string]string{}
	for _, it := range items {
		opts = append(opts, huh.NewOption(it.label, it.id).Selected(selected[it.id]))
		labels[it.id] = it.label
	}
	var picked []string
	if err := ui.Form(huh.NewGroup(huh.NewMultiSelect[string]().
		Title("Which conversations trigger this agent?").
		Description("Type / to filter, space to select, enter to confirm. Only top-level messages trigger; thread replies steer.").
		Options(opts...).Filterable(true).Height(14).Value(&picked))); err != nil {
		return nil, err
	}
	if len(picked) == 0 {
		return nil, errors.New("pick at least one conversation")
	}
	var out []config.Ref
	for _, id := range picked {
		out = append(out, config.Ref{ID: id, Name: cleanLabel(labels[id])})
	}
	return out, nil
}

func cleanLabel(s string) string {
	s = strings.TrimSpace(strings.TrimLeft(s, "#@✎⧉🔒 "))
	if i := strings.Index(s, "  ("); i > 0 {
		s = s[:i]
	}
	return s
}

// pickSenders chooses who may trigger the agent, offering recent senders (people, bots, workflows).
func pickSenders(ctx context.Context, dir *slack.Directory, ws *config.Workspace, convs []config.Ref, current []config.Ref) ([]config.Ref, error) {
	mode := "anyone"
	switch {
	case len(current) == 1 && current[0].ID == config.SelfRef:
		mode = "me"
	case len(current) > 0:
		mode = "pick"
	}
	if err := ui.Form(huh.NewGroup(huh.NewSelect[string]().Title("Who can trigger it?").Options(
		huh.NewOption("Anyone", "anyone"),
		huh.NewOption("Only me", "me"),
		huh.NewOption("Specific people, bots or workflows…", "pick"),
	).Value(&mode))); err != nil {
		return nil, err
	}
	switch mode {
	case "anyone":
		return nil, nil
	case "me":
		return []config.Ref{{ID: config.SelfRef, Name: ws.UserName}}, nil
	}

	type sender struct{ id, name, kind string }
	found := map[string]sender{}
	_ = ui.Spin("Looking at recent messages for senders", func(ctx context.Context) error {
		for _, c := range convs {
			page, err := dir.API().History(ctx, c.ID, slack.HistoryOptions{Limit: 200})
			if err != nil {
				continue
			}
			for i := range page.Messages {
				m := &page.Messages[i]
				id := m.SenderID()
				if id == "" || found[id].id != "" {
					continue
				}
				kind := "person"
				switch {
				case m.User == "":
					kind = "bot/workflow"
				case m.BotID != "" || m.BotProfile != nil:
					kind = "app"
				}
				found[id] = sender{id, dir.SenderName(ctx, m), kind}
			}
		}
		return nil
	})
	selected := map[string]bool{}
	for _, r := range current {
		selected[r.ID] = true
	}
	var list []sender
	for _, s := range found {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return strings.ToLower(list[i].name) < strings.ToLower(list[j].name) })
	opts := []huh.Option[string]{huh.NewOption(ws.UserName+" (me)", config.SelfRef).Selected(selected[config.SelfRef])}
	names := map[string]string{config.SelfRef: ws.UserName}
	for _, s := range list {
		if s.id == ws.UserID {
			continue
		}
		opts = append(opts, huh.NewOption(fmt.Sprintf("%s  %s", s.name, ui.Dim("("+s.kind+", "+s.id+")")), s.id).Selected(selected[s.id]))
		names[s.id] = s.name
	}
	var picked []string
	var extra string
	for _, r := range current {
		if _, ok := names[r.ID]; !ok {
			extra = strings.TrimPrefix(extra+", "+r.ID, ", ")
			names[r.ID] = r.Name
		}
	}
	if err := ui.Form(huh.NewGroup(
		huh.NewMultiSelect[string]().Title("Pick senders").
			Description("People and bots/workflows that posted recently in the chosen conversations.").
			Options(opts...).Filterable(true).Height(12).Value(&picked),
		huh.NewInput().Title("Other IDs (optional)").Description("Comma-separated user (U…), bot (B…) or app (A…) IDs.").Value(&extra),
	)); err != nil {
		return nil, err
	}
	var out []config.Ref
	for _, id := range append(picked, splitList(extra)...) {
		out = append(out, config.Ref{ID: id, Name: names[id]})
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// keepDefault lets an empty answer keep the current value in line-by-line (accessible)
// mode, where prompts show the value instead of pre-filling the field.
func keepDefault(current string, validate func(string) error) func(string) error {
	return func(s string) error {
		if strings.TrimSpace(s) == "" && current != "" && ui.Accessible() {
			return nil
		}
		return validate(s)
	}
}

func validateDir(s string) error {
	p := expandHome(strings.TrimSpace(s))
	st, err := os.Stat(p)
	if err != nil || !st.IsDir() {
		return errors.New("directory does not exist")
	}
	return nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

func isGitRepo(dir string) bool {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return true
		}
		if filepath.Dir(d) == d {
			return false
		}
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
