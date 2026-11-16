package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/daffirds/agent-shift/internal/config"
	"github.com/daffirds/agent-shift/internal/link"
	"github.com/daffirds/agent-shift/internal/secret"
	"github.com/daffirds/agent-shift/internal/slack"
	"github.com/daffirds/agent-shift/internal/ui"
)

func slackCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "slack", Short: "Connected Slack workspaces (connect them in `agent-shift ui`)"}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List connected workspaces and check their tokens",
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				if len(cfg.Workspaces) == 0 {
					ui.Warn("No workspace connected. Connect one in %s", ui.Code("agent-shift ui"))
					return nil
				}
				for _, w := range cfg.Workspaces {
					if _, err := checkWorkspace(cmd.Context(), &w); err != nil {
						ui.Fail("%s (%s): %v", ui.Bold(w.Name), w.TeamID, err)
					} else {
						ui.OK("%s (%s) as %s", ui.Bold(w.Name), w.TeamID, w.UserName)
					}
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "remove <team-id|name>",
			Short: "Stop using a workspace and delete its token from the keychain",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				var ws *config.Workspace
				for i := range cfg.Workspaces {
					if w := &cfg.Workspaces[i]; w.TeamID == args[0] || strings.EqualFold(w.Name, args[0]) {
						ws = w
					}
				}
				if ws == nil {
					return fmt.Errorf("no workspace %q", args[0])
				}
				var users []string
				for _, a := range cfg.Agents {
					if a.Trigger.Workspace == ws.TeamID {
						users = append(users, a.Name)
					}
				}
				if len(users) > 0 {
					return fmt.Errorf("agents still use %s: %s (remove them first)", ws.Name, strings.Join(users, ", "))
				}
				cfg.RemoveWorkspace(ws.TeamID)
				if err := saveConfig(cmd.Context(), cfg); err != nil {
					return err
				}
				return secret.DeleteWorkspace(ws.TeamID)
			},
		},
		findCmd(),
	)
	return cmd
}

// findCmd looks up conversation and user IDs, handy when editing config.yaml by hand.
func findCmd() *cobra.Command {
	var team string
	cmd := &cobra.Command{
		Use:   "find [query]",
		Short: "Look up IDs of your channels, DMs and people (for config.yaml)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if len(cfg.Workspaces) == 0 {
				return errors.New("no workspace connected; connect one in `agent-shift ui`")
			}
			ws := &cfg.Workspaces[0]
			if team != "" {
				if ws = cfg.Workspace(team); ws == nil {
					return fmt.Errorf("no workspace %q", team)
				}
			}
			api, err := link.Slack(ws)
			if err != nil {
				return err
			}
			dir := slack.NewDirectory(api)
			q := ""
			if len(args) == 1 {
				q = strings.ToLower(strings.TrimLeft(args[0], "#@"))
			}
			var convs []slack.Conversation
			var users []slack.User
			err = ui.Spin("Searching "+ws.Name, func(ctx context.Context) error {
				var err error
				if convs, err = dir.MyConversations(ctx); err != nil {
					return err
				}
				users, err = dir.AllUsers(ctx)
				return err
			})
			if err != nil {
				return err
			}
			names := map[string]string{}
			for _, u := range users {
				names[u.ID] = u.Display()
			}
			ui.Heading("Conversations")
			for _, c := range convs {
				label := "#" + c.Name
				switch {
				case c.IsIM && c.User == ws.UserID:
					label = "notes to self (" + names[c.User] + ")"
				case c.IsIM:
					label = "DM with " + firstNonEmpty(names[c.User], c.User)
				case c.IsMPIM:
					label = "group DM " + strings.TrimPrefix(c.Name, "mpdm-")
				}
				if q == "" || strings.Contains(strings.ToLower(label), q) || strings.Contains(strings.ToLower(c.ID), q) {
					ui.Line("  %-12s %-16s %s", c.ID, c.Kind(), label)
				}
			}
			if q != "" {
				ui.Heading("People")
				for _, u := range users {
					if strings.Contains(strings.ToLower(u.Display()+" "+u.Name+" "+u.RealName+" "+u.Profile.Email), q) {
						ui.Line("  %-12s %-24s %s", u.ID, u.Display(), ui.Dim("@"+u.Name+" "+u.Profile.Email))
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&team, "workspace", "", "team ID (default: the first connected workspace)")
	return cmd
}

// checkWorkspace verifies that the workspace's stored token still acts as the same Slack
// user, and returns the scopes it lacks.
func checkWorkspace(ctx context.Context, w *config.Workspace) ([]string, error) {
	token, err := link.Token(w.TeamID)
	if err != nil {
		return nil, err
	}
	id, err := link.Identify(ctx, token)
	if err != nil {
		return nil, err
	}
	if id.Workspace.TeamID != w.TeamID || id.Workspace.UserID != w.UserID {
		return nil, fmt.Errorf("the stored token now acts as %s in %s; reconnect in `agent-shift ui`", id.Workspace.UserName, id.Workspace.Name)
	}
	return id.MissingScopes, nil
}
