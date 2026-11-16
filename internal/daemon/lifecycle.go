package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/daffirds/agent-shift/internal/app"
	"github.com/daffirds/agent-shift/internal/autostart"
	"github.com/daffirds/agent-shift/internal/proc"
)

// Start starts the daemon in the background (through the login item when autostart manages
// it) and waits until it answers. started is false when it was already running.
func Start(ctx context.Context) (rt *Runtime, started bool, err error) {
	if c, err := Connect(); err == nil {
		return c.Runtime(), false, nil
	}
	if err := app.EnsureHome(); err != nil {
		return nil, false, err
	}
	if autostart.Managed() {
		if err := autostart.Start(); err != nil {
			return nil, false, err
		}
	} else {
		exe, err := os.Executable()
		if err != nil {
			return nil, false, err
		}
		out, err := os.OpenFile(app.LogPath()+".stderr", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, false, err
		}
		defer out.Close()
		cmd := exec.Command(exe, "run", "--background")
		cmd.Stdout, cmd.Stderr = out, out
		p, err := proc.StartDetached(cmd)
		if err != nil {
			return nil, false, fmt.Errorf("start daemon: %w", err)
		}
		_ = p.Release()
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := Connect(); err == nil {
			return c.Runtime(), true, nil
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	tail, _ := os.ReadFile(app.LogPath() + ".stderr")
	return nil, false, fmt.Errorf("daemon did not come up; see %s\n%s", app.LogPath(), strings.TrimSpace(string(tail)))
}

// Stop asks a running daemon to shut down and waits for it to exit. stopped is false when it
// was not running.
func Stop(ctx context.Context) (stopped bool, err error) {
	c, err := Connect()
	if err != nil {
		return false, nil
	}
	pid := c.Runtime().PID
	if err := c.Shutdown(ctx); err != nil {
		return false, err
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if !proc.Alive(pid) {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return false, fmt.Errorf("daemon (pid %d) did not exit in time", pid)
}
