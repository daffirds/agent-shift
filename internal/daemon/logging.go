package daemon

import (
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/lmittmann/tint"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/daffirds/agent-shift/internal/app"
)

// NewLogger logs to a rotating file (~/.agent-shift/logs/daemon.log) and, when console is
// true, also to stderr in a readable, colored format.
func NewLogger(level string, console bool) (*slog.Logger, io.Closer) {
	lvl := parseLevel(level)
	file := &lumberjack.Logger{Filename: app.LogPath(), MaxSize: 10, MaxBackups: 5, LocalTime: true}
	handlers := []slog.Handler{slog.NewTextHandler(file, &slog.HandlerOptions{Level: lvl})}
	if console {
		handlers = append(handlers, tint.NewHandler(os.Stderr, &tint.Options{
			Level: lvl, TimeFormat: "15:04:05", NoColor: !isTerminal(os.Stderr),
		}))
	}
	return slog.New(slog.NewMultiHandler(handlers...)), file
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func isTerminal(f *os.File) bool { return term.IsTerminal(f.Fd()) }
