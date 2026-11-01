// Package app holds build metadata and the on-disk layout of agent-shift's home directory.
package app

import (
	"os"
	"path/filepath"
)

// Name is the binary and product name.
const Name = "agent-shift"

// Version is overridden at build time via -ldflags "-X github.com/daffirds/agent-shift/internal/app.Version=...".
var Version = "dev"

// HomeEnv overrides the home directory (useful for running several isolated instances).
const HomeEnv = "AGENT_SHIFT_HOME"

// Home returns the agent-shift home directory (~/.agent-shift unless AGENT_SHIFT_HOME is set).
func Home() string {
	if h := os.Getenv(HomeEnv); h != "" {
		return h
	}
	dir, err := os.UserHomeDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "."+Name)
}

// ConfigPath is the YAML config written by `agent-shift setup`.
func ConfigPath() string { return filepath.Join(Home(), "config.yaml") }

// CredentialsPath is the fallback secret store used when the OS keychain is unavailable.
func CredentialsPath() string { return filepath.Join(Home(), "credentials.json") }

// SessionsDir holds one directory per session (state, transcript, downloaded files).
func SessionsDir() string { return filepath.Join(Home(), "sessions") }

// WorktreesDir holds git worktrees created for agents with `worktree: true`.
func WorktreesDir() string { return filepath.Join(Home(), "worktrees") }

// LogsDir holds the daemon log files.
func LogsDir() string { return filepath.Join(Home(), "logs") }

// LogPath is the daemon's main log file.
func LogPath() string { return filepath.Join(LogsDir(), "daemon.log") }

// StatePath records, per workspace, until when Slack activity was received, so the daemon
// can catch up on what it missed while offline.
func StatePath() string { return filepath.Join(Home(), "state.json") }

// RuntimePath describes the running daemon (pid, control address, token).
func RuntimePath() string { return filepath.Join(Home(), "daemon.json") }

// LockPath guards against running two daemons at once.
func LockPath() string { return filepath.Join(Home(), "daemon.lock") }

// EnsureHome creates the home directory tree with private permissions.
func EnsureHome() error {
	for _, d := range []string{Home(), SessionsDir(), LogsDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}
