package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/daffirds/agent-shift/internal/app"
	"github.com/daffirds/agent-shift/internal/autostart"
	"github.com/daffirds/agent-shift/internal/config"
	"github.com/daffirds/agent-shift/internal/daemon"
	"github.com/daffirds/agent-shift/internal/ui"
)

func setupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Guided setup: check CLIs, connect Slack, create an agent, start the daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ui.Banner(app.Name+" setup", "Slack threads → Claude Code / Codex on this machine")

			ui.Step(1, 4, "Coding agent CLIs")
			if checkCLIs(cfg) == 0 {
				return errors.New("install Claude Code or Codex and log in, then run setup again")
			}

			ui.Step(2, 4, "Slack")
			connect := len(cfg.Workspaces) == 0
			for _, w := range cfg.Workspaces {
				if err := checkWorkspace(ctx, &w); err != nil {
					ui.Fail("%s: %v", w.Name, err)
				} else {
					ui.OK("%s as %s", ui.Bold(w.Name), w.UserName)
				}
			}
			if !connect {
				if connect, err = ui.Confirm("Connect another workspace (or re-enter tokens)?", "", false); err != nil {
					return err
				}
			}
			if connect {
				ws, err := connectWorkspace(ctx, true)
				if err != nil {
					return err
				}
				cfg.UpsertWorkspace(*ws)
				if err := saveConfig(ctx, cfg); err != nil {
					return err
				}
			}

			ui.Step(3, 4, "Agent")
			for _, a := range cfg.Agents {
				ui.OK("%s (%s) → %s", ui.Bold(a.Name), a.Engine, ui.Dim(a.Workdir))
			}
			create := len(cfg.Agents) == 0
			if !create {
				if create, err = ui.Confirm("Create another agent?", "", false); err != nil {
					return err
				}
			}
			var created *config.Agent
			if create {
				if created, err = agentForm(ctx, cfg, nil); err != nil {
					return err
				}
				cfg.UpsertAgent("", *created)
				if err := saveConfig(ctx, cfg); err != nil {
					return err
				}
			}

			ui.Step(4, 4, "Run")
			if c, err := daemon.Connect(); err == nil {
				ui.OK("Daemon already running (pid %d) and up to date", c.Runtime().PID)
			} else {
				start, err := ui.Confirm("Start the daemon now?", "Runs in the background; stop it with `agent-shift stop`.", true)
				if err != nil {
					return err
				}
				if start {
					if err := startDaemon(ctx, false); err != nil {
						return err
					}
				}
			}
			if !autostart.Get().Installed {
				enable, err := ui.Confirm("Start agent-shift automatically when you log in?", "", false)
				if err != nil {
					return err
				}
				if enable {
					if err := enableAutostart(ctx); err != nil {
						ui.Warn("autostart: %v", err)
					}
				}
			}

			ui.Heading("You're set")
			if created != nil {
				hint := "post a top-level message"
				if len(created.Trigger.Contains) > 0 {
					hint += fmt.Sprintf(" containing %q", created.Trigger.Contains[0])
				}
				var where []string
				for _, c := range created.Trigger.Conversations {
					where = append(where, c.Name)
				}
				ui.Info("Try it: %s in %s.", hint, strings.Join(where, " or "))
			}
			ui.Info("%s   overview      %s   daemon log", ui.Code("agent-shift status"), ui.Code("agent-shift logs -f"))
			ui.Info("%s   sessions      %s   checks", ui.Code("agent-shift sessions"), ui.Code("agent-shift doctor"))
			return nil
		},
	}
}
