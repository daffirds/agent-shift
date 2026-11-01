// Package ui holds terminal styling and interactive form helpers shared by the CLI.
package ui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"charm.land/huh/v2"
	"charm.land/huh/v2/spinner"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"

	"github.com/daffirds/agent-shift/internal/proc"
)

var (
	accent  = lipgloss.Color("#8B5CF6")
	green   = lipgloss.Color("#22C55E")
	yellow  = lipgloss.Color("#EAB308")
	red     = lipgloss.Color("#EF4444")
	gray    = lipgloss.Color("#8A8F98")
	bold    = lipgloss.NewStyle().Bold(true)
	title   = lipgloss.NewStyle().Bold(true).Foreground(accent)
	dim     = lipgloss.NewStyle().Foreground(gray)
	code    = lipgloss.NewStyle().Foreground(accent)
	okMark  = lipgloss.NewStyle().Foreground(green).Render("✓")
	warnMk  = lipgloss.NewStyle().Foreground(yellow).Render("!")
	errMark = lipgloss.NewStyle().Foreground(red).Render("✗")
	banner  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(accent).Padding(0, 1)
)

func Bold(s string) string { return bold.Render(s) }
func Dim(s string) string  { return dim.Render(s) }
func Code(s string) string { return code.Render(s) }

// Banner prints a header line like " agent-shift setup ".
func Banner(text, subtitle string) {
	lipgloss.Println()
	lipgloss.Println(banner.Render(text) + "  " + dim.Render(subtitle))
	lipgloss.Println()
}

// Step prints a numbered section heading.
func Step(n, total int, text string) {
	lipgloss.Println()
	lipgloss.Println(title.Render(fmt.Sprintf("Step %d/%d", n, total)) + dim.Render(" · ") + bold.Render(text))
}

func Heading(text string) {
	lipgloss.Println()
	lipgloss.Println(title.Render(text))
}

func OK(format string, a ...any)   { lipgloss.Println("  " + okMark + " " + fmt.Sprintf(format, a...)) }
func Warn(format string, a ...any) { lipgloss.Println("  " + warnMk + " " + fmt.Sprintf(format, a...)) }
func Fail(format string, a ...any) {
	lipgloss.Println("  " + errMark + " " + fmt.Sprintf(format, a...))
}
func Info(format string, a ...any) { lipgloss.Println("    " + fmt.Sprintf(format, a...)) }
func Line(format string, a ...any) { lipgloss.Println(fmt.Sprintf(format, a...)) }

// KV prints aligned "key  value" rows.
func KV(rows [][2]string) {
	w := 0
	for _, r := range rows {
		w = max(w, len(r[0]))
	}
	for _, r := range rows {
		lipgloss.Println("  " + dim.Render(r[0]+strings.Repeat(" ", w-len(r[0]))) + "  " + r[1])
	}
}

// Accessible reports whether forms should use plain line-by-line prompts: requested via
// AGENT_SHIFT_ACCESSIBLE=1 / ACCESSIBLE=1, or forced because stdin/stdout is not a terminal.
func Accessible() bool {
	if os.Getenv("AGENT_SHIFT_ACCESSIBLE") != "" || os.Getenv("ACCESSIBLE") != "" {
		return true
	}
	return !IsTerminal(os.Stdin) || !IsTerminal(os.Stdout)
}

// IsTerminal reports whether f is an interactive console (NUL / pipes are not).
func IsTerminal(f *os.File) bool { return term.IsTerminal(f.Fd()) }

// Form runs the given groups as one form with the agent-shift theme.
func Form(groups ...*huh.Group) error {
	f := huh.NewForm(groups...).WithTheme(huh.ThemeFunc(huh.ThemeCharm)).WithAccessible(Accessible())
	if !IsTerminal(os.Stdin) {
		f = f.WithInput(lineReader{os.Stdin}) // piped answers (scripts): one per prompt
	}
	err := f.Run()
	if err == huh.ErrUserAborted {
		return ErrAborted
	}
	return err
}

// ErrAborted is returned when the user cancels a form (Ctrl+C / Esc).
var ErrAborted = fmt.Errorf("cancelled")

// lineReader returns at most one line per Read. huh's accessible prompts each wrap the
// input in a new buffered scanner; without this, the first prompt would swallow every
// piped answer meant for the following ones.
type lineReader struct{ r io.Reader }

func (l lineReader) Read(p []byte) (int, error) {
	n := 0
	var b [1]byte
	for n < len(p) {
		m, err := l.r.Read(b[:])
		if m == 1 {
			p[n] = b[0]
			n++
			if b[0] == '\n' {
				return n, nil
			}
		}
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// Spin runs fn while showing a spinner.
func Spin(text string, fn func(ctx context.Context) error) error {
	if Accessible() {
		lipgloss.Println("  " + dim.Render(text+"…"))
		return fn(context.Background())
	}
	return spinner.New().Title(" " + text).ActionWithErr(fn).Run()
}

// Confirm asks a yes/no question.
func Confirm(question, description string, def bool) (bool, error) {
	v := def
	err := Form(huh.NewGroup(huh.NewConfirm().Title(question).Description(description).Value(&v)))
	return v, err
}

// OpenBrowser opens url in the default browser (best-effort).
func OpenBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	proc.HideWindow(cmd)
	return cmd.Start()
}
