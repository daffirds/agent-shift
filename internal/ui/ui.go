// Package ui holds terminal styling helpers shared by the CLI.
package ui

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"charm.land/lipgloss/v2"

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

// Spin runs fn after saying what it is doing.
func Spin(text string, fn func(ctx context.Context) error) error {
	lipgloss.Println("  " + dim.Render(text+"…"))
	return fn(context.Background())
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
