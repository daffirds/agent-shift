// Package link builds the Slack clients a configuration describes from the tokens in the OS
// keychain, and identifies the Slack user behind a token.
package link

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/daffirds/agent-shift/internal/config"
	"github.com/daffirds/agent-shift/internal/secret"
	"github.com/daffirds/agent-shift/internal/slack"
)

// ErrNoToken means a workspace's Slack token is missing from the keychain.
var ErrNoToken = errors.New("no Slack token stored for this workspace; reconnect it in `agent-shift ui`")

// Token returns a workspace's stored user token.
func Token(teamID string) (string, error) {
	t, err := secret.Get(secret.SlackToken(teamID))
	if errors.Is(err, secret.ErrNotFound) || (err == nil && t == "") {
		return "", ErrNoToken
	}
	return t, err
}

// Slack returns a client acting as the workspace's user.
func Slack(w *config.Workspace) (*slack.Client, error) {
	t, err := Token(w.TeamID)
	if err != nil {
		return nil, err
	}
	return slack.NewClient(t), nil
}

// Identity is who a Slack token acts as.
type Identity struct {
	Workspace     config.Workspace
	MissingScopes []string
}

// CheckToken rejects strings that cannot be a Slack user token.
func CheckToken(token string) error {
	switch {
	case token == "":
		return errors.New("paste the User OAuth Token (xoxp-…)")
	case strings.HasPrefix(token, "xoxb-"):
		return errors.New("that is a bot token (xoxb-…); agent-shift needs the User OAuth Token (xoxp-…)")
	case strings.HasPrefix(token, "xapp-"):
		return errors.New("that is an app-level token (xapp-…); agent-shift needs the User OAuth Token (xoxp-…)")
	case !strings.HasPrefix(token, "xoxp-"):
		return errors.New("a User OAuth Token starts with xoxp-")
	}
	return nil
}

// Identify asks Slack who a token is.
func Identify(ctx context.Context, token string) (*Identity, error) {
	if err := CheckToken(token); err != nil {
		return nil, err
	}
	c := slack.NewClient(token)
	auth, err := c.AuthTest(ctx)
	if err != nil {
		if slack.IsCode(err, "invalid_auth", "not_authed", "token_revoked", "account_inactive") {
			return nil, errors.New("Slack rejected this token (revoked, or from a reinstalled app?)")
		}
		return nil, fmt.Errorf("slack auth.test: %w", err)
	}
	if auth.UserID == "" || auth.TeamID == "" {
		return nil, errors.New("this token did not report a Slack user")
	}
	w := config.Workspace{TeamID: auth.TeamID, Name: auth.Team, URL: auth.URL, UserID: auth.UserID, UserName: auth.User, AppID: auth.AppID}
	if u, err := c.UserInfo(ctx, auth.UserID); err == nil {
		w.UserName = u.Display()
	}
	id := &Identity{Workspace: w}
	if scopes, err := c.Scopes(ctx); err == nil {
		id.MissingScopes = slack.MissingScopes(scopes)
	}
	return id, nil
}
