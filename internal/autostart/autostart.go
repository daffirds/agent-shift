// Package autostart registers the daemon to start at login: a launchd LaunchAgent on
// macOS, a per-user Run entry on Windows. Both run in the user's session, so the CLIs'
// own logins (Keychain / credential files) keep working.
package autostart

// Status describes the current registration.
type Status struct {
	Installed bool
	Detail    string // plist path or registry command
}

// Install registers exe to run the daemon at login. pathEnv is the PATH the daemon should
// see (login items get a minimal PATH, which would hide the claude/codex CLIs).
func Install(exe, pathEnv string) (string, error) { return install(exe, pathEnv) }

// Uninstall removes the registration.
func Uninstall() error { return uninstall() }

// Get reports whether autostart is registered.
func Get() Status { return get() }

// Managed reports whether the OS service manager supervises the daemon (macOS launchd);
// then start/stop go through it instead of spawning processes directly.
func Managed() bool { return managed() }

// Start asks the service manager to start the daemon (only when Managed).
func Start() error { return startManaged() }

// Stop asks the service manager to stop the daemon (only when Managed).
func Stop() error { return stopManaged() }
