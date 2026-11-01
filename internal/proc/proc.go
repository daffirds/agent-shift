// Package proc wraps OS-specific process handling: hidden child processes on Windows,
// process groups on macOS, killing whole process trees, and detaching the daemon.
package proc

import (
	"os"
	"os/exec"
	"time"
)

// Group is a started child process whose whole tree can be killed at once.
type Group struct {
	Cmd *exec.Cmd
	sys groupSys
}

// Start starts cmd as the root of a new process group (a Job Object on Windows),
// without a console window.
func Start(cmd *exec.Cmd) (*Group, error) {
	prepare(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	g := &Group{Cmd: cmd}
	g.attach()
	return g, nil
}

// Kill terminates the process and all of its descendants.
func (g *Group) Kill() { g.kill() }

// Stop waits up to grace for the process to exit on its own (callers typically close stdin first),
// then kills the tree.
func (g *Group) Stop(done <-chan struct{}, grace time.Duration) {
	select {
	case <-done:
	case <-time.After(grace):
	}
	g.kill()
}

// StartDetached starts cmd so it outlives the current process and terminal (used by `agent-shift start`).
func StartDetached(cmd *exec.Cmd) (*os.Process, error) { return startDetached(cmd) }

// Alive reports whether a process with this pid exists.
func Alive(pid int) bool { return alive(pid) }

// HideWindow configures a short-lived helper command (git, taskkill…) to run without a console window.
func HideWindow(cmd *exec.Cmd) { hideWindow(cmd) }
