//go:build !windows

package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

type groupSys struct{}

func prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func hideWindow(*exec.Cmd) {}

func (g *Group) attach() {}

func (g *Group) kill() {
	if g.Cmd.Process == nil {
		return
	}
	// Negative pid signals the whole process group created by Setpgid.
	_ = syscall.Kill(-g.Cmd.Process.Pid, syscall.SIGKILL)
	_ = g.Cmd.Process.Kill()
}

func startDetached(cmd *exec.Cmd) (*os.Process, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
