package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/daffirds/agent-shift/internal/app"
	"github.com/daffirds/agent-shift/internal/autostart"
	"github.com/daffirds/agent-shift/internal/daemon"
	"github.com/daffirds/agent-shift/internal/proc"
	"github.com/daffirds/agent-shift/internal/session"
	"github.com/daffirds/agent-shift/internal/ui"
)

func runCmd() *cobra.Command {
	var service, background bool
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the daemon in the foreground (Ctrl+C to stop)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			log, closer := daemon.NewLogger(cfg.Daemon.LogLevel, !service && !background)
			defer closer.Close()
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			err = daemon.Run(ctx, log)
			if errors.Is(err, daemon.ErrAlreadyRunning) && service {
				return nil // launchd: don't crash-loop while another instance runs
			}
			if err != nil {
				log.Error("daemon stopped", "err", err)
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&service, "service", false, "started by the OS service manager (logs to file only)")
	cmd.Flags().BoolVar(&background, "background", false, "started by `agent-shift start` (logs to file only)")
	_ = cmd.Flags().MarkHidden("service")
	_ = cmd.Flags().MarkHidden("background")
	return cmd
}

func startCmd() *cobra.Command {
	var quiet bool
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the daemon in the background",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return startDaemon(cmd.Context(), quiet)
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "print nothing on success")
	return cmd
}

func startDaemon(ctx context.Context, quiet bool) error {
	if c, err := daemon.Connect(); err == nil {
		if !quiet {
			ui.OK("Already running (pid %d)", c.Runtime().PID)
		}
		return nil
	}
	if err := app.EnsureHome(); err != nil {
		return err
	}
	if autostart.Managed() {
		if err := autostart.Start(); err != nil {
			return err
		}
	} else {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(app.LogPath()+".stderr", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		defer out.Close()
		cmd := exec.Command(exe, "run", "--background")
		cmd.Stdout, cmd.Stderr = out, out
		p, err := proc.StartDetached(cmd)
		if err != nil {
			return fmt.Errorf("start daemon: %w", err)
		}
		_ = p.Release()
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := daemon.Connect(); err == nil {
			if !quiet {
				ui.OK("Daemon started (pid %d) — logs: %s", c.Runtime().PID, ui.Dim(app.LogPath()))
			}
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	tail, _ := os.ReadFile(app.LogPath() + ".stderr")
	return fmt.Errorf("daemon did not come up; see %s\n%s", app.LogPath(), strings.TrimSpace(string(tail)))
}

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the background daemon (sessions are kept and resume later)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return stopDaemon(cmd.Context(), false)
		},
	}
}

func stopDaemon(ctx context.Context, quiet bool) error {
	c, err := daemon.Connect()
	if err != nil {
		if !quiet {
			ui.OK("Not running")
		}
		return nil
	}
	pid := c.Runtime().PID
	if err := c.Shutdown(ctx); err != nil {
		return err
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if !proc.Alive(pid) {
			if !quiet {
				ui.OK("Daemon stopped")
			}
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("daemon (pid %d) did not exit in time", pid)
}

func restartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the background daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := stopDaemon(cmd.Context(), true); err != nil {
				return err
			}
			return startDaemon(cmd.Context(), false)
		},
	}
}

func reloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reload",
		Short: "Make the running daemon re-read config.yaml",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := daemon.Connect()
			if err != nil {
				return err
			}
			res, err := c.Reload(cmd.Context())
			if err != nil {
				return err
			}
			ui.OK("Reloaded")
			for _, w := range res.Warnings {
				ui.Warn("%s", w)
			}
			return nil
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon, Slack connection and session status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := daemon.Connect()
			if err != nil {
				ui.Fail("Daemon not running — start it with %s", ui.Code("agent-shift start"))
				if a := autostart.Get(); a.Installed {
					ui.Info("autostart is enabled (%s)", a.Detail)
				}
				return nil
			}
			st, err := c.Status(cmd.Context())
			if err != nil {
				return err
			}
			ui.Heading("Daemon")
			ui.KV([][2]string{
				{"pid", fmt.Sprint(st.PID)},
				{"version", st.Version},
				{"up", time.Since(st.Started).Round(time.Second).String()},
				{"logs", app.LogPath()},
			})
			ui.Heading("Slack")
			if len(st.Workspaces) == 0 {
				ui.Warn("no workspace connected — run %s", ui.Code("agent-shift slack connect"))
			}
			for _, w := range st.Workspaces {
				if w.Connected {
					ui.OK("%s as %s", ui.Bold(w.Name), w.User)
				} else {
					ui.Fail("%s: %s", ui.Bold(w.Name), firstNonEmpty(w.Error, "connecting…"))
				}
				if line := catchUpLine(w.CatchUp); line != "" {
					ui.Info("%s", ui.Dim(line))
				}
			}
			ui.Heading("Agents")
			if len(st.Agents) == 0 {
				ui.Warn("no agents — run %s", ui.Code("agent-shift agents add"))
			}
			for _, a := range st.Agents {
				state := "on"
				if a.Disabled {
					state = "off"
				}
				ui.Line("  %-20s %-7s %-4s %s", a.Name, a.Engine, state, ui.Dim(a.Workdir))
			}
			ui.Heading("Sessions")
			total := 0
			for _, s := range []session.State{session.StateRunning, session.StateIdle, session.StateQueued, session.StateSleeping, session.StateFailed} {
				if n := st.Sessions[s]; n > 0 {
					ui.Line("  %-9s %d", s, n)
					total += n
				}
			}
			if total == 0 {
				ui.Line("  %s", ui.Dim("none active"))
			}
			return nil
		},
	}
}

func logsCmd() *cobra.Command {
	var follow bool
	var lines int
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Print the daemon log",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return tailFile(cmd.Context(), app.LogPath(), lines, follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new lines")
	cmd.Flags().IntVarP(&lines, "lines", "n", 60, "number of lines to show")
	return cmd
}

// tailFile prints the last n lines of path, optionally following appends.
func tailFile(ctx context.Context, path string, n int, follow bool) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("no log yet at %s", path)
	}
	defer f.Close()
	var ring []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		ring = append(ring, sc.Text())
		if len(ring) > n {
			ring = ring[1:]
		}
	}
	for _, l := range ring {
		fmt.Println(l)
	}
	if !follow {
		return nil
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			fmt.Print(line)
		}
		if err == io.EOF {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(300 * time.Millisecond):
			}
			if st, serr := os.Stat(path); serr == nil {
				if pos, _ := f.Seek(0, io.SeekCurrent); st.Size() < pos { // rotated
					f.Close()
					if f, err = os.Open(path); err != nil {
						return err
					}
					r = bufio.NewReader(f)
				}
			}
			continue
		}
		if err != nil {
			return err
		}
	}
}

// catchUpLine describes the last replay after downtime, if there was a real gap or it did something.
func catchUpLine(cu *daemon.CatchUpStatus) string {
	if cu == nil {
		return ""
	}
	offline := cu.At.Sub(cu.From)
	switch {
	case cu.Error != "":
		return fmt.Sprintf("catching up on %s offline — incomplete, retrying: %s", span(offline), cu.Error)
	case cu.Sessions+cu.Replies == 0 && offline < 5*time.Minute:
		return ""
	}
	return fmt.Sprintf("caught up %s on %s offline: %s, %s", ago(cu.At), span(offline),
		plural(cu.Sessions, "new session", "new sessions"), plural(cu.Replies, "thread reply", "thread replies"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
