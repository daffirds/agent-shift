package slack

import (
	"net/url"
	"strings"
)

// UserScopes are the user-token scopes agent-shift needs to read and act as the user.
var UserScopes = []string{
	"channels:history", "channels:read",
	"groups:history", "groups:read",
	"im:history", "im:read", "im:write",
	"mpim:history", "mpim:read", "mpim:write",
	"chat:write",
	"reactions:read", "reactions:write",
	"files:read", "files:write",
	"users:read", "users:read.email",
	"search:read",
	"team:read",
}

// UserEvents are delivered for conversations the user is a member of.
var UserEvents = []string{
	"message.channels", "message.groups", "message.im", "message.mpim",
	"reaction_added", "reaction_removed",
}

// Manifest returns the Slack app manifest (YAML) for a user-token-only Socket Mode app.
func Manifest(appName string) string {
	var b strings.Builder
	b.WriteString("_metadata:\n  major_version: 1\n")
	b.WriteString("display_information:\n")
	b.WriteString("  name: " + yamlQuote(appName) + "\n")
	b.WriteString("  description: Relays Slack threads to local Claude Code / Codex sessions, acting as you.\n")
	b.WriteString("  background_color: \"#1d1c1d\"\n")
	b.WriteString("oauth_config:\n  scopes:\n    user:\n")
	for _, s := range UserScopes {
		b.WriteString("      - " + s + "\n")
	}
	b.WriteString("settings:\n  event_subscriptions:\n    user_events:\n")
	for _, e := range UserEvents {
		b.WriteString("      - " + e + "\n")
	}
	b.WriteString("  org_deploy_enabled: false\n")
	b.WriteString("  socket_mode_enabled: true\n")
	b.WriteString("  token_rotation_enabled: false\n")
	return b.String()
}

// CreateAppURL opens Slack's "create app from manifest" flow with the manifest pre-filled.
func CreateAppURL(manifest string) string {
	return "https://api.slack.com/apps?new_app=1&manifest_yaml=" + url.QueryEscape(manifest)
}

// AppSettingsURL links to a page of an app's settings (e.g. "general", "install-on-team").
func AppSettingsURL(appID, page string) string {
	if appID == "" {
		return "https://api.slack.com/apps"
	}
	return "https://api.slack.com/apps/" + appID + "/" + page
}

func yamlQuote(s string) string {
	return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(s) + "\""
}
