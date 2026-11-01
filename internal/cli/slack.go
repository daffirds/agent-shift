package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/daffirds/agent-shift/internal/app"
	"github.com/daffirds/agent-shift/internal/config"
	"github.com/daffirds/agent-shift/internal/secret"
	"github.com/daffirds/agent-shift/internal/slack"
	"github.com/daffirds/agent-shift/internal/ui"
)

func slackCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "slack", Short: "Connect and manage Slack workspaces"}
	var noBrowser bool
	connect := &cobra.Command{
		Use:   "connect",
		Short: "Connect your Slack account (guided: create app, paste two tokens)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ws, err := connectWorkspace(cmd.Context(), !noBrowser)
			if err != nil {
				return err
			}
			cfg.UpsertWorkspace(*ws)
			return saveConfig(cmd.Context(), cfg)
		},
	}
	connect.Flags().BoolVar(&noBrowser, "no-browser", false, "print the app-creation link instead of opening it")
	cmd.AddCommand(
		connect,
		&cobra.Command{
			Use:   "list",
			Short: "List connected workspaces and check their tokens",
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				if len(cfg.Workspaces) == 0 {
					ui.Warn("No workspace connected. Run %s", ui.Code("agent-shift slack connect"))
					return nil
				}
				for _, w := range cfg.Workspaces {
					if err := checkWorkspace(cmd.Context(), &w); err != nil {
						ui.Fail("%s (%s): %v", ui.Bold(w.Name), w.TeamID, err)
					} else {
						ui.OK("%s (%s) as %s %s", ui.Bold(w.Name), w.TeamID, w.UserName, ui.Dim(w.URL))
					}
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "remove <team-id|name>",
			Short: "Disconnect a workspace and delete its tokens",
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
				team := ws.TeamID
				cfg.RemoveWorkspace(team)
				if err := secret.Delete(team); err != nil {
					ui.Warn("could not delete tokens: %v", err)
				}
				return saveConfig(cmd.Context(), cfg)
			},
		},
		&cobra.Command{
			Use:   "manifest",
			Short: "Print the Slack app manifest (to create or update the app by hand)",
			Run: func(*cobra.Command, []string) {
				fmt.Print(slack.Manifest(app.Name))
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
				return errors.New("no workspace connected; run agent-shift slack connect")
			}
			ws := &cfg.Workspaces[0]
			if team != "" {
				if ws = cfg.Workspace(team); ws == nil {
					return fmt.Errorf("no workspace %q", team)
				}
			}
			tok, err := secret.Get(ws.TeamID, secret.UserToken)
			if err != nil {
				return err
			}
			dir := slack.NewDirectory(slack.NewClient(tok))
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

// checkWorkspace verifies both tokens of a workspace.
func checkWorkspace(ctx context.Context, w *config.Workspace) error {
	userTok, err := secret.Get(w.TeamID, secret.UserToken)
	if err != nil {
		return fmt.Errorf("user token: %w", err)
	}
	appTok, err := secret.Get(w.TeamID, secret.AppToken)
	if err != nil {
		return fmt.Errorf("app token: %w", err)
	}
	if _, err := slack.NewClient(userTok).AuthTest(ctx); err != nil {
		return fmt.Errorf("user token: %w", err)
	}
	if _, err := slack.NewClient(appTok).ConnectionsOpen(ctx); err != nil {
		return fmt.Errorf("app token: %w", err)
	}
	return nil
}

// connectWorkspace walks the user through creating the Slack app and pasting its two tokens.
func connectWorkspace(ctx context.Context, openBrowser bool) (*config.Workspace, error) {
	appName, mode := app.Name, "create"
	if err := ui.Form(huh.NewGroup(
		huh.NewNote().Title("Connect Slack").Description(
			"agent-shift acts as *your* Slack account (not a bot). It needs a small private Slack app\n"+
				"in your workspace that grants your account's token and a Socket Mode connection.\n"+
				"No public URL or server is needed — everything runs on this machine."),
		huh.NewSelect[string]().Title("Slack app").Options(
			huh.NewOption("Create it now (opens Slack with everything pre-filled)", "create"),
			huh.NewOption("I already created it — just paste its tokens", "existing"),
		).Value(&mode),
	)); err != nil {
		return nil, err
	}

	if mode == "create" {
		if err := ui.Form(huh.NewGroup(
			huh.NewInput().Title("Slack app name").Description("Shown in your workspace's app list.").Value(&appName).
				Validate(keepDefault(appName, func(s string) error {
					if strings.TrimSpace(s) == "" || len(s) > 35 {
						return errors.New("1–35 characters")
					}
					return nil
				})),
		)); err != nil {
			return nil, err
		}
		appName = strings.TrimSpace(appName)
		if appName == "" || len(appName) > 35 {
			appName = app.Name
		}
		manifest := slack.Manifest(appName)
		manifestPath := filepath.Join(app.Home(), "slack-app-manifest.yaml")
		_ = app.EnsureHome()
		_ = os.WriteFile(manifestPath, []byte(manifest), 0o600)
		createURL := slack.CreateAppURL(manifest)

		ui.Heading("1. Create the Slack app")
		ui.Info("Open this link, pick your workspace → Next → Create (manifest also saved to %s):", ui.Dim(manifestPath))
		ui.Info("%s", ui.Dim(createURL))
		if openBrowser {
			_ = ui.OpenBrowser(createURL)
		}
	} else {
		ui.Heading("1. Open your app")
		ui.Info("Go to %s and open the app you created for agent-shift.", ui.Code("https://api.slack.com/apps"))
	}

	ui.Heading("2. App-level token (for Socket Mode)")
	ui.Info("On the app's %s page: App-Level Tokens → %s →", ui.Bold("Basic Information"), ui.Bold("Generate Token and Scopes"))
	ui.Info("name it anything, %s → %s, then %s and copy the %s token.", ui.Bold("Add Scope"), ui.Code("connections:write"), ui.Bold("Generate"), ui.Code("xapp-…"))
	appTok, err := askToken("App-level token", "xapp-", func(ctx context.Context, tok string) error {
		_, err := slack.NewClient(tok).ConnectionsOpen(ctx)
		if slack.IsCode(err, "invalid_auth", "not_authed") {
			return errors.New("Slack rejected this token")
		}
		return err
	})
	if err != nil {
		return nil, err
	}

	ui.Heading("3. Install the app and copy your user token")
	ui.Info("Open %s (left sidebar) → %s → %s.", ui.Bold("OAuth & Permissions"), ui.Bold("Install to <workspace>"), ui.Bold("Allow"))
	ui.Info("Then copy the %s (%s).", ui.Bold("User OAuth Token"), ui.Code("xoxp-…"))
	ui.Info("%s", ui.Dim("If your workspace requires admin approval, request it there and continue once approved."))
	var auth *slack.AuthInfo
	userTok, err := askToken("User OAuth token", "xoxp-", func(ctx context.Context, tok string) error {
		a, err := slack.NewClient(tok).AuthTest(ctx)
		if err != nil {
			return err
		}
		auth = a
		return nil
	})
	if err != nil {
		return nil, err
	}

	api := slack.NewClient(userTok)
	ws := &config.Workspace{TeamID: auth.TeamID, Name: auth.Team, URL: auth.URL, UserID: auth.UserID, UserName: auth.User}
	if u, err := api.UserInfo(ctx, auth.UserID); err == nil {
		ws.UserName = u.Display()
	}
	if scopes, err := api.Scopes(ctx); err == nil && len(scopes) > 0 {
		var missing []string
		for _, s := range slack.UserScopes {
			if !slices.Contains(scopes, s) {
				missing = append(missing, s)
			}
		}
		if len(missing) > 0 {
			ui.Warn("The token is missing scopes: %s", strings.Join(missing, ", "))
			ui.Info("Update the app with %s, reinstall it, and run this again.", ui.Code("agent-shift slack manifest"))
		}
	}
	if err := errors.Join(secret.Set(ws.TeamID, secret.AppToken, appTok), secret.Set(ws.TeamID, secret.UserToken, userTok)); err != nil {
		return nil, fmt.Errorf("store tokens: %w", err)
	}
	ui.OK("Connected to %s as %s", ui.Bold(ws.Name), ui.Bold(ws.UserName))
	return ws, nil
}

// askToken prompts for a secret until it has the right prefix and passes check.
func askToken(label, prefix string, check func(context.Context, string) error) (string, error) {
	const attempts = 5
	var rejected string
	for attempt := 1; ; attempt++ {
		var tok string
		err := ui.Form(huh.NewGroup(huh.NewInput().Title(label).Placeholder(prefix + "…").
			EchoMode(huh.EchoModePassword).Value(&tok).
			Validate(func(s string) error {
				if !strings.HasPrefix(strings.TrimSpace(s), prefix) {
					return fmt.Errorf("should start with %s", prefix)
				}
				return nil
			})))
		if err != nil {
			return "", err
		}
		tok = strings.TrimSpace(tok)
		if tok == "" || tok == rejected {
			return "", fmt.Errorf("no new %s entered", strings.ToLower(label)) // e.g. input closed
		}
		err = ui.Spin("Checking token", func(ctx context.Context) error { return check(ctx, tok) })
		if err == nil {
			ui.OK("%s looks good", label)
			return tok, nil
		}
		if attempt == attempts {
			return "", fmt.Errorf("%s: %w", strings.ToLower(label), err)
		}
		rejected = tok
		ui.Fail("%v — try again", err)
	}
}
