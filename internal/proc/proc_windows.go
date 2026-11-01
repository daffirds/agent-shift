package proc

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const createNoWindow = 0x08000000

type groupSys struct {
	job windows.Handle
}

func prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow | windows.CREATE_NEW_PROCESS_GROUP
}

func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

// attach puts the child in a Job Object that kills every member when closed,
// so descendants (shells, git, node…) never outlive the session.
func (g *Group) attach() {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(g.Cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return
	}
	g.sys.job = job
}

func (g *Group) kill() {
	if g.sys.job != 0 {
		_ = windows.TerminateJobObject(g.sys.job, 1)
		windows.CloseHandle(g.sys.job)
		g.sys.job = 0
	}
	if g.Cmd.Process != nil {
		// Fallback when the job couldn't be created (e.g. nested job restrictions).
		kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(g.Cmd.Process.Pid))
		hideWindow(kill)
		_ = kill.Run()
		_ = g.Cmd.Process.Kill()
	}
}

func startDetached(cmd *exec.Cmd) (*os.Process, error) {
	// A hidden console of its own (children inherit it, so no windows pop up), its own
	// process group (Ctrl+C in the launching terminal doesn't reach it), and no ties to
	// the launching terminal's job object when that job allows breaking away.
	base := uint32(createNoWindow | windows.CREATE_NEW_PROCESS_GROUP)
	attempt := func(flags uint32) (*exec.Cmd, error) {
		c := exec.Command(cmd.Path, cmd.Args[1:]...)
		c.Env, c.Dir, c.Stdin, c.Stdout, c.Stderr = cmd.Env, cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr
		c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
		return c, c.Start()
	}
	c, err := attempt(base | windows.CREATE_BREAKAWAY_FROM_JOB)
	if err != nil {
		if c, err = attempt(base); err != nil {
			return nil, err
		}
	}
	return c.Process, nil
}

func alive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}
