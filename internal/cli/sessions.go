package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/daffirds/agent-shift/internal/daemon"
	"github.com/daffirds/agent-shift/internal/session"
	"github.com/daffirds/agent-shift/internal/ui"
)

func sessionsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "sessions", Aliases: []string{"session", "s"}, Short: "Inspect and control agent sessions"}
	var all bool
	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List sessions (live ones by default)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			recs, err := sessionRecords(cmd.Context())
			if err != nil {
				return err
			}
			shown := 0
			for _, r := range recs {
				if !all && !r.State.Live() {
					continue
				}
				shown++
				ui.Line("%s  %-8s  %-16s %-8s %s", ui.Bold(r.ID), stateLabel(r.State), r.Agent, ago(r.LastActive), ui.Dim(slackLabel(&r)))
			}
			if shown == 0 {
				ui.Line("%s", ui.Dim("No sessions."+map[bool]string{false: " (use --all to include ended ones)"}[all]))
			}
			return nil
		},
	}
	list.Flags().BoolVarP(&all, "all", "a", false, "include ended sessions")

	cmd.AddCommand(list,
		&cobra.Command{
			Use: "show <id>", Short: "Show a session's details", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				r, err := findSession(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				ui.Heading("Session " + r.ID)
				ui.KV([][2]string{
					{"state", stateLabel(r.State)},
					{"agent", r.Agent + " (" + r.Engine + ")"},
					{"thread", slackLabel(r)},
					{"link", r.Permalink},
					{"started by", r.From},
					{"workdir", r.Workdir},
					{"engine session", r.EngineID},
					{"turns", fmt.Sprint(r.Turns)},
					{"created", r.CreatedAt.Format(time.RFC1123)},
					{"last active", ago(r.LastActive)},
					{"last error", firstNonEmpty(r.LastError, "-")},
					{"files", r.Dir()},
				})
				return nil
			},
		},
		&cobra.Command{
			Use: "stop <id>", Short: "End a session (its thread stops steering it)", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := daemon.Connect()
				if err != nil {
					return err
				}
				r, err := findSession(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if err := c.StopSession(cmd.Context(), r.ID); err != nil {
					return err
				}
				ui.OK("Session %s ended", r.ID)
				return nil
			},
		},
		&cobra.Command{
			Use: "interrupt <id>", Short: "Interrupt the session's current turn", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := daemon.Connect()
				if err != nil {
					return err
				}
				r, err := findSession(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if err := c.InterruptSession(cmd.Context(), r.ID); err != nil {
					return err
				}
				ui.OK("Interrupt sent to %s", r.ID)
				return nil
			},
		},
		sessionLogsCmd(),
		pruneCmd(),
	)
	return cmd
}

func sessionLogsCmd() *cobra.Command {
	var raw, follow bool
	var lines int
	cmd := &cobra.Command{
		Use: "logs <id>", Short: "Show what a session did (prompts, replies, tool calls)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := findSession(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if raw || follow {
				return tailFile(cmd.Context(), r.TranscriptPath(), lines, follow)
			}
			return printTranscript(r.TranscriptPath(), lines)
		},
	}
	cmd.Flags().BoolVar(&raw, "raw", false, "print raw protocol JSON lines")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow raw output")
	cmd.Flags().IntVarP(&lines, "lines", "n", 200, "max entries")
	return cmd
}

// printTranscript summarises a transcript.jsonl: user prompts, assistant text and tool calls.
func printTranscript(path string, limit int) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("no transcript yet")
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64<<20)
	for sc.Scan() {
		var rec struct {
			T   string          `json:"t"`
			Dir string          `json:"dir"`
			Msg json.RawMessage `json:"msg"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		stamp := rec.T
		if t, err := time.Parse(time.RFC3339Nano, rec.T); err == nil {
			stamp = t.Local().Format("15:04:05")
		}
		for _, line := range summarize(rec.Dir, rec.Msg) {
			out = append(out, ui.Dim(stamp)+" "+line)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	for _, l := range out {
		ui.Line("%s", l)
	}
	return nil
}

// summarize extracts human-readable lines from one protocol message (Claude or Codex).
func summarize(dir string, raw json.RawMessage) []string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	str := func(v any) string { s, _ := v.(string); return s }
	short := func(s string, n int) string {
		s = strings.Join(strings.Fields(s), " ")
		if len(s) > n {
			return s[:n] + "…"
		}
		return s
	}
	var lines []string
	switch {
	case dir == "out" && str(m["type"]) == "user": // claude prompt
		if msg, ok := m["message"].(map[string]any); ok {
			lines = append(lines, ui.Bold("→ ")+short(str(msg["content"]), 300))
		}
	case dir == "out" && (str(m["method"]) == "turn/start" || str(m["method"]) == "turn/steer"): // codex prompt
		if p, ok := m["params"].(map[string]any); ok {
			if in, ok := p["input"].([]any); ok && len(in) > 0 {
				if first, ok := in[0].(map[string]any); ok {
					lines = append(lines, ui.Bold("→ ")+short(str(first["text"]), 300))
				}
			}
		}
	case str(m["type"]) == "assistant":
		msg, _ := m["message"].(map[string]any)
		content, _ := msg["content"].([]any)
		for _, c := range content {
			cm, _ := c.(map[string]any)
			switch str(cm["type"]) {
			case "text":
				lines = append(lines, "  "+short(str(cm["text"]), 400))
			case "tool_use":
				in, _ := json.Marshal(cm["input"])
				lines = append(lines, ui.Dim("  ⚙ "+str(cm["name"])+" "+short(string(in), 160)))
			}
		}
	case str(m["type"]) == "result":
		lines = append(lines, ui.Dim(fmt.Sprintf("  ■ turn done (%s)", str(m["subtype"]))))
	case str(m["method"]) == "item/completed":
		p, _ := m["params"].(map[string]any)
		item, _ := p["item"].(map[string]any)
		switch str(item["type"]) {
		case "agentMessage":
			lines = append(lines, "  "+short(str(item["text"]), 400))
		case "commandExecution":
			lines = append(lines, ui.Dim("  ⚙ shell "+short(str(item["command"]), 160)))
		case "mcpToolCall":
			args, _ := json.Marshal(item["arguments"])
			lines = append(lines, ui.Dim("  ⚙ "+str(item["server"])+"."+str(item["tool"])+" "+short(string(args), 160)))
		case "fileChange":
			lines = append(lines, ui.Dim("  ⚙ edited files"))
		}
	case str(m["method"]) == "turn/completed":
		p, _ := m["params"].(map[string]any)
		turn, _ := p["turn"].(map[string]any)
		lines = append(lines, ui.Dim("  ■ turn "+str(turn["status"])))
	}
	return lines
}

func pruneCmd() *cobra.Command {
	var olderThan time.Duration
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete ended sessions' files and git worktrees",
		RunE: func(cmd *cobra.Command, _ []string) error {
			recs, err := session.LoadRecords()
			if err != nil {
				return err
			}
			n := 0
			for _, r := range recs {
				if r.State.Live() || time.Since(r.LastActive) < olderThan {
					continue
				}
				if r.Worktree != "" {
					if err := session.RemoveWorktree(cmd.Context(), r.AgentConfig.Workdir, r.Worktree); err != nil {
						ui.Warn("%s: %v", r.ID, err)
					}
				}
				if err := os.RemoveAll(r.Dir()); err != nil {
					ui.Warn("%s: %v", r.ID, err)
					continue
				}
				n++
			}
			ui.OK("Pruned %d ended session(s)", n)
			return nil
		},
	}
	cmd.Flags().DurationVar(&olderThan, "older-than", 0, "only prune sessions inactive for at least this long (e.g. 72h)")
	return cmd
}

// sessionRecords prefers the daemon's live view and falls back to the files on disk.
func sessionRecords(ctx context.Context) ([]session.Record, error) {
	if c, err := daemon.Connect(); err == nil {
		if recs, err := c.Sessions(ctx); err == nil {
			return recs, nil
		}
	}
	recs, err := session.LoadRecords()
	if err != nil {
		return nil, err
	}
	out := make([]session.Record, len(recs))
	for i, r := range recs {
		out[i] = *r
	}
	return out, nil
}

func findSession(ctx context.Context, prefix string) (*session.Record, error) {
	recs, err := sessionRecords(ctx)
	if err != nil {
		return nil, err
	}
	var hit *session.Record
	for i := range recs {
		r := &recs[i]
		if r.ID == prefix {
			return r, nil
		}
		if strings.HasPrefix(r.ID, prefix) {
			if hit != nil {
				return nil, fmt.Errorf("%q matches several sessions", prefix)
			}
			hit = r
		}
	}
	if hit == nil {
		return nil, fmt.Errorf("no session %q", prefix)
	}
	return hit, nil
}

func stateLabel(s session.State) string {
	switch s {
	case session.StateRunning:
		return "● running"
	case session.StateIdle:
		return "○ idle"
	case session.StateQueued:
		return "◌ queued"
	case session.StateSleeping:
		return "◦ sleeping"
	case session.StateFailed:
		return "✗ failed"
	default:
		return "· ended"
	}
}

func slackLabel(r *session.Record) string {
	s := firstNonEmpty(r.ChannelName, r.Channel)
	if r.Title != "" {
		s += " — " + r.Title
	}
	return s
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	if d := time.Since(t); d >= time.Minute {
		return span(d) + " ago"
	}
	return "just now"
}

// span formats a duration coarsely: 45s, 12m, 15h, 3d.
func span(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
