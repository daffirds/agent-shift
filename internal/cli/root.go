// Package cli implements the agent-shift command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/daffirds/agent-shift/internal/app"
	"github.com/daffirds/agent-shift/internal/config"
	"github.com/daffirds/agent-shift/internal/daemon"
	"github.com/daffirds/agent-shift/internal/ui"
)

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	root := &cobra.Command{
		Use:   app.Name,
		Short: "Relay Slack threads to Claude Code / Codex sessions running on this machine",
		Long: app.Name + ` listens to your Slack account and starts a local Claude Code or Codex session
whenever a message matches one of your agents. Replies and reactions in the thread are
steered into the same session, and the agent answers through Slack tools.

Get started with:  ` + app.Name + ` setup`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       app.Version,
	}
	root.AddCommand(
		setupCmd(),
		runCmd(), startCmd(), stopCmd(), restartCmd(), statusCmd(), logsCmd(), reloadCmd(),
		slackCmd(), agentsCmd(), sessionsCmd(), autostartCmd(), doctorCmd(),
	)
	if err := root.ExecuteContext(context.Background()); err != nil {
		if errors.Is(err, ui.ErrAborted) {
			ui.Line("%s", ui.Dim("Cancelled."))
			return 130
		}
		ui.Fail("%v", err)
		return 1
	}
	return 0
}

// loadConfig reads the config, treating a missing file as empty.
func loadConfig() (*config.Config, error) {
	cfg, err := config.Load()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return cfg, nil
}

// saveConfig validates, writes, and asks a running daemon to pick up the change.
func saveConfig(ctx context.Context, cfg *config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := app.EnsureHome(); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	ui.OK("Saved %s", ui.Dim(app.ConfigPath()))
	c, err := daemon.Connect()
	if err != nil {
		return nil
	}
	res, err := c.Reload(ctx)
	if err != nil {
		return fmt.Errorf("daemon rejected the new config: %w", err)
	}
	ui.OK("Daemon reloaded")
	for _, w := range res.Warnings {
		ui.Warn("%s", w)
	}
	return nil
}
