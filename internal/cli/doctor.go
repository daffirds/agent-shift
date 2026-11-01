package cli

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/daffirds/agent-shift/internal/app"
	"github.com/daffirds/agent-shift/internal/autostart"
	"github.com/daffirds/agent-shift/internal/config"
	"github.com/daffirds/agent-shift/internal/daemon"
	"github.com/daffirds/agent-shift/internal/engine"
	"github.com/daffirds/agent-shift/internal/secret"
	"github.com/daffirds/agent-shift/internal/slack"
	"github.com/daffirds/agent-shift/internal/ui"
)

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check CLIs, logins, Slack tokens/scopes and the daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ui.Banner(app.Name+" doctor", app.Version+" · "+runtime.GOOS+"/"+runtime.GOARCH)
			problems := 0

			ui.Heading("Coding agent CLIs")
			if checkCLIs(cfg) == 0 {
				problems++
			}
			for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
				if os.Getenv(k) != "" {
					ui.Info("%s", ui.Dim(k+" is set; agent-shift hides it from Claude so your subscription is used (opt back in via engines.claude.env)."))
				}
			}

			ui.Heading("Slack")
			if len(cfg.Workspaces) == 0 {
				ui.Fail("no workspace connected — run %s", ui.Code("agent-shift slack connect"))
				problems++
			}
			for _, w := range cfg.Workspaces {
				if err := checkWorkspace(cmd.Context(), &w); err != nil {
					ui.Fail("%s: %v", w.Name, err)
					problems++
					continue
				}
				ui.OK("%s: tokens valid (as %s)", w.Name, w.UserName)
				if tok, err := secret.Get(w.TeamID, secret.UserToken); err == nil {
					if scopes, err := slack.NewClient(tok).Scopes(cmd.Context()); err == nil {
						var missing []string
						for _, s := range slack.UserScopes {
							if !slices.Contains(scopes, s) {
								missing = append(missing, s)
							}
						}
						if len(missing) > 0 {
							ui.Warn("%s: missing scopes %s (update the app from `agent-shift slack manifest` and reinstall)", w.Name, strings.Join(missing, ", "))
							problems++
						}
					}
				}
			}

			ui.Heading("Agents")
			if err := cfg.Validate(); err != nil {
				ui.Fail("config: %v", err)
				problems++
			}
			for _, w := range cfg.Warnings() {
				ui.Warn("%s", w)
				problems++
			}
			if len(cfg.Agents) == 0 {
				ui.Warn("no agents — run %s", ui.Code("agent-shift agents add"))
			}
			for _, a := range cfg.Agents {
				if _, err := engine.Find(a.Engine, binaryOf(cfg, a.Engine).Path); err != nil {
					ui.Fail("%s: %s CLI not found", a.Name, a.Engine)
					problems++
				} else if !a.Disabled {
					ui.OK("%s (%s) → %s", a.Name, a.Engine, a.Workdir)
				}
			}

			ui.Heading("Daemon")
			if c, err := daemon.Connect(); err == nil {
				ui.OK("running (pid %d)", c.Runtime().PID)
			} else {
				ui.Warn("not running — %s", ui.Code("agent-shift start"))
			}
			if a := autostart.Get(); a.Installed {
				ui.OK("starts at login (%s)", ui.Dim(a.Detail))
			} else {
				ui.Info("%s", ui.Dim("not set to start at login (agent-shift autostart enable)"))
			}

			lineBreak()
			if problems > 0 {
				return fmt.Errorf("%d problem(s) found", problems)
			}
			ui.OK("All good")
			return nil
		},
	}
}

func lineBreak() { ui.Line("") }

// checkCLIs prints each engine's install and login status and returns how many are usable.
func checkCLIs(cfg *config.Config) int {
	usable := 0
	for _, e := range []struct{ id, label, install, login string }{
		{config.EngineClaude, "Claude Code", "https://claude.com/claude-code", "claude auth login"},
		{config.EngineCodex, "Codex", "https://developers.openai.com/codex/cli", "codex login"},
	} {
		bin, err := engine.Find(e.id, binaryOf(cfg, e.id).Path)
		if err != nil {
			ui.Info("%s", ui.Dim(fmt.Sprintf("%s not installed (%s)", e.label, e.install)))
			continue
		}
		version := engine.Version(bin)
		ok, detail := engine.AuthStatus(e.id, bin)
		if !ok {
			ui.Warn("%s %s found but not logged in — run %s", e.label, version, ui.Code(e.login))
			continue
		}
		ui.OK("%s %s %s", e.label, version, ui.Dim("("+detail+")"))
		usable++
	}
	return usable
}

func autostartCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "autostart", Short: "Start the daemon automatically when you log in"}
	cmd.AddCommand(
		&cobra.Command{
			Use: "enable", Short: "Start agent-shift at login",
			RunE: func(cmd *cobra.Command, _ []string) error { return enableAutostart(cmd.Context()) },
		},
		&cobra.Command{
			Use: "disable", Short: "Don't start agent-shift at login",
			RunE: func(*cobra.Command, []string) error {
				if err := autostart.Uninstall(); err != nil {
					return err
				}
				ui.OK("Autostart disabled")
				return nil
			},
		},
		&cobra.Command{
			Use: "status", Short: "Show autostart status",
			Run: func(*cobra.Command, []string) {
				if a := autostart.Get(); a.Installed {
					ui.OK("Enabled: %s", a.Detail)
				} else {
					ui.Line("Disabled")
				}
			},
		},
	)
	return cmd
}

func enableAutostart(ctx context.Context) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	running := false
	if _, err := daemon.Connect(); err == nil {
		running = true
	}
	if running && runtime.GOOS == "darwin" {
		// launchd will own the daemon from now on.
		if err := stopDaemon(ctx, true); err != nil {
			return err
		}
	}
	where, err := autostart.Install(exe, os.Getenv("PATH"))
	if err != nil {
		return err
	}
	ui.OK("agent-shift will start when you log in %s", ui.Dim("("+where+")"))
	if runtime.GOOS == "darwin" || !running {
		return startDaemon(ctx, false)
	}
	return nil
}
