// Package codex drives `codex app-server` (JSON-RPC over stdio), the same protocol the
// Codex IDE extension uses. Messages arriving mid-turn are injected with turn/steer.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/daffirds/agent-shift/internal/app"
	"github.com/daffirds/agent-shift/internal/config"
	"github.com/daffirds/agent-shift/internal/engine"
	"github.com/daffirds/agent-shift/internal/proc"
)

type Engine struct{}

func (Engine) Name() string { return config.EngineCodex }

func (Engine) Start(ctx context.Context, spec engine.Spec) (engine.Session, error) {
	s, err := launch(ctx, spec)
	if err != nil {
		return nil, err
	}
	if err := s.openThread(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// Models lists the models of `codex app-server` (model/list) with their reasoning efforts,
// after an entry for Codex's own default.
func (Engine) Models(ctx context.Context, spec engine.Spec) ([]engine.Model, error) {
	s, err := launch(ctx, spec)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	list := []engine.Model{{Name: "Default", Description: "Codex's own setting"}}
	seen := map[string]bool{}
	params := map[string]any{}
	for range 20 {
		var resp struct {
			Data []struct {
				Model                     string `json:"model"`
				DisplayName               string `json:"displayName"`
				Description               string `json:"description"`
				Hidden                    bool   `json:"hidden"`
				IsDefault                 bool   `json:"isDefault"`
				DefaultReasoningEffort    string `json:"defaultReasoningEffort"`
				SupportedReasoningEfforts []struct {
					ReasoningEffort string `json:"reasoningEffort"`
					Description     string `json:"description"`
				} `json:"supportedReasoningEfforts"`
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		if err := s.rpc.call(ctx, "model/list", params, &resp); err != nil {
			return nil, fmt.Errorf("codex model/list: %w%s", err, s.stderr.Suffix())
		}
		for _, m := range resp.Data {
			if m.Hidden || m.Model == "" || seen[m.Model] {
				continue
			}
			seen[m.Model] = true
			md := engine.Model{ID: m.Model, Name: m.DisplayName, Description: m.Description, DefaultEffort: m.DefaultReasoningEffort}
			for _, e := range m.SupportedReasoningEfforts {
				md.Efforts = append(md.Efforts, engine.Effort{Level: e.ReasoningEffort, Description: e.Description})
			}
			if m.IsDefault { // without a model, sessions get the built-in default unless config.toml sets one
				list[0].Description = "Codex's own setting (out of the box: " + m.DisplayName + ")"
				list[0].Efforts, list[0].DefaultEffort = md.Efforts, md.DefaultEffort
			}
			list = append(list, md)
		}
		if resp.NextCursor == "" {
			break
		}
		params["cursor"] = resp.NextCursor
	}
	return list, nil
}

// launch starts `codex app-server` and completes the initialize handshake.
func launch(ctx context.Context, spec engine.Spec) (*session, error) {
	args := []string{"app-server"}
	args = append(args, spec.Args...)
	for _, kv := range spec.ExtraArgs {
		args = append(args, "-c", kv)
	}
	cmd := exec.Command(spec.Binary, args...)
	cmd.Dir = spec.Workdir
	cmd.Env = childEnv(spec)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	s := &session{
		spec:   spec,
		stdin:  stdin,
		events: make(chan engine.Event, 256),
		done:   make(chan struct{}),
	}
	s.rpc = &rpc{w: stdin, transcript: spec.Transcript, pending: map[int64]chan rpcResult{},
		onNotify: s.onNotify, onRequest: s.onRequest}
	cmd.Stderr = &s.stderr
	g, err := proc.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("start codex: %w", err)
	}
	s.group, s.pid = g, cmd.Process.Pid
	readDone := make(chan struct{})
	go func() { s.rpc.readLoop(stdout); close(readDone) }()
	go func() {
		err := cmd.Wait()
		<-readDone
		if err != nil {
			s.setErr(fmt.Errorf("codex exited: %w%s", err, s.stderr.Suffix()))
		}
		s.mu.Lock()
		s.closed = true
		close(s.events)
		s.mu.Unlock()
		close(s.done)
	}()

	if err := s.initialize(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *session) initialize(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	err := s.rpc.call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "agent_shift", "title": app.Name, "version": app.Version},
		"capabilities": map[string]any{"experimentalApi": true},
	}, nil)
	if err != nil {
		return fmt.Errorf("codex initialize: %w%s", err, s.stderr.Suffix())
	}
	return s.rpc.notify("initialized", map[string]any{})
}

// openThread starts the session's thread, or resumes it.
func (s *session) openThread(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	// Merged with the user's own config.toml (verified against codex 0.166).
	cfg := map[string]any{"mcp_servers": map[string]any{
		// "approve": our Slack tools never wait for an approval nobody can give.
		s.spec.MCP.Name: map[string]any{"url": s.spec.MCP.URL, "tool_timeout_sec": 300, "default_tools_approval_mode": "approve"},
	}}
	if s.spec.Effort != "" {
		cfg["model_reasoning_effort"] = s.spec.Effort
	}
	params := map[string]any{
		"cwd":                   s.spec.Workdir,
		"developerInstructions": s.spec.Instructions,
		"config":                cfg,
		"serviceName":           app.Name,
	}
	switch s.spec.Autonomy {
	case config.AutonomyFull:
		params["approvalPolicy"], params["sandbox"] = "never", "danger-full-access"
	case config.AutonomyAuto:
		params["approvalPolicy"], params["sandbox"] = "on-request", "workspace-write"
		params["approvalsReviewer"] = "auto_review"
	default:
		params["approvalPolicy"], params["sandbox"] = "never", "read-only"
	}
	if s.spec.Model != "" {
		params["model"] = s.spec.Model
	}
	var resp struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoningEffort"`
	}
	method := "thread/start"
	if s.spec.ResumeID != "" {
		method = "thread/resume"
		params["threadId"] = s.spec.ResumeID
		params["excludeTurns"] = true
	}
	if err := s.rpc.call(ctx, method, params, &resp); err != nil {
		return fmt.Errorf("codex %s: %w%s", method, err, s.stderr.Suffix())
	}
	s.threadID = resp.Thread.ID
	s.spec.Log.Debug("codex started", "pid", s.pid, "thread", s.threadID, "resume", s.spec.ResumeID != "",
		"model", resp.Model, "effort", resp.ReasoningEffort)
	return nil
}

type session struct {
	spec     engine.Spec
	rpc      *rpc
	group    *proc.Group
	pid      int
	stdin    io.WriteCloser
	stderr   engine.StderrTail
	threadID string

	mu            sync.Mutex
	turnID        string // active turn, "" when idle
	lastCompleted string
	closed        bool
	err           error
	events        chan engine.Event
	done          chan struct{}
}

func (s *session) ID() string                  { return s.threadID }
func (s *session) Events() <-chan engine.Event { return s.events }
func (s *session) Done() <-chan struct{}       { return s.done }

func (s *session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *session) setErr(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

func (s *session) emit(ev engine.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.events <- ev:
	default:
	}
}

func textInput(text string) []map[string]any {
	return []map[string]any{{"type": "text", "text": text}}
}

// Send steers the active turn, or starts a new one. turn/steer and turn/start can race
// with a turn that is just starting or ending; both fail cleanly (an RPC error means the
// input was not taken), so a short retry loop settles the race without double delivery.
func (s *session) Send(ctx context.Context, text string) error {
	var lastErr error
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 400 * time.Millisecond):
			}
		}
		s.mu.Lock()
		turn := s.turnID
		s.mu.Unlock()
		if turn != "" {
			err := s.rpc.call(ctx, "turn/steer", map[string]any{
				"threadId": s.threadID, "expectedTurnId": turn, "input": textInput(text),
			}, nil)
			if err == nil {
				return nil
			}
			lastErr = err // most likely the turn just finished: start a new one
		}
		var resp struct {
			Turn struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turn"`
		}
		err := s.rpc.call(ctx, "turn/start", map[string]any{"threadId": s.threadID, "input": textInput(text)}, &resp)
		if err == nil {
			s.mu.Lock()
			if resp.Turn.ID != s.lastCompleted && resp.Turn.Status == "inProgress" {
				s.turnID = resp.Turn.ID
			}
			s.mu.Unlock()
			return nil
		}
		lastErr = err
		if errors.Is(err, engine.ErrClosed) || ctx.Err() != nil {
			return err
		}
		// A turn we haven't heard about yet may be active: retry, steering it next time.
	}
	return lastErr
}

func (s *session) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	turn := s.turnID
	s.mu.Unlock()
	if turn == "" {
		return nil
	}
	return s.rpc.call(ctx, "turn/interrupt", map[string]any{"threadId": s.threadID, "turnId": turn}, nil)
}

func (s *session) Close() error {
	_ = s.stdin.Close() // app-server exits on EOF
	s.group.Stop(s.done, 10*time.Second)
	<-s.done
	return nil
}

type turnPayload struct {
	ThreadID string `json:"threadId"`
	Turn     struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"turn"`
}

type itemPayload struct {
	ThreadID string `json:"threadId"`
	Item     struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Phase   string `json:"phase"`
		Command string `json:"command"`
		Server  string `json:"server"`
		Tool    string `json:"tool"`
		Query   string `json:"query"`
		Status  string `json:"status"`
		Error   *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"item"`
}

func (s *session) onNotify(method string, params json.RawMessage) {
	switch method {
	case "turn/started":
		var p turnPayload
		if json.Unmarshal(params, &p) != nil || p.ThreadID != s.threadID {
			return
		}
		s.mu.Lock()
		s.turnID = p.Turn.ID
		s.mu.Unlock()
		s.emit(engine.Event{Kind: engine.TurnStarted})
	case "turn/completed":
		var p turnPayload
		if json.Unmarshal(params, &p) != nil || p.ThreadID != s.threadID {
			return
		}
		s.mu.Lock()
		if s.turnID == p.Turn.ID {
			s.turnID = ""
		}
		s.lastCompleted = p.Turn.ID
		s.mu.Unlock()
		ev := engine.Event{Kind: engine.TurnCompleted}
		switch {
		case p.Turn.Error != nil && p.Turn.Error.Message != "":
			ev.Error = p.Turn.Error.Message
		case p.Turn.Status == "failed" || p.Turn.Status == "interrupted":
			ev.Error = "turn " + p.Turn.Status
		}
		s.emit(ev)
	case "item/started":
		var p itemPayload
		if json.Unmarshal(params, &p) != nil || p.ThreadID != s.threadID {
			return
		}
		switch p.Item.Type {
		case "commandExecution":
			s.emit(engine.Event{Kind: engine.ToolCall, Text: "shell " + p.Item.Command})
		case "mcpToolCall":
			s.emit(engine.Event{Kind: engine.ToolCall, Text: p.Item.Server + "." + p.Item.Tool})
		case "fileChange":
			s.emit(engine.Event{Kind: engine.ToolCall, Text: "edit files"})
		case "webSearch":
			s.emit(engine.Event{Kind: engine.ToolCall, Text: "web search " + p.Item.Query})
		}
	case "item/completed":
		var p itemPayload
		if json.Unmarshal(params, &p) != nil || p.ThreadID != s.threadID {
			return
		}
		switch {
		case p.Item.Type == "agentMessage" && strings.TrimSpace(p.Item.Text) != "":
			s.emit(engine.Event{Kind: engine.AssistantText, Text: p.Item.Text})
		case p.Item.Type == "mcpToolCall" && p.Item.Error != nil:
			s.emit(engine.Event{Kind: engine.Notice, Text: fmt.Sprintf("tool %s.%s failed: %s", p.Item.Server, p.Item.Tool, p.Item.Error.Message)})
		}
	case "error":
		var p struct {
			ThreadID  string `json:"threadId"`
			WillRetry bool   `json:"willRetry"`
			Error     struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(params, &p) == nil && (p.ThreadID == "" || p.ThreadID == s.threadID) {
			s.emit(engine.Event{Kind: engine.Notice, Text: fmt.Sprintf("codex error (retrying=%v): %s", p.WillRetry, p.Error.Message)})
		}
	case "mcpServer/startupStatus/updated":
		var p struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if json.Unmarshal(params, &p) == nil && p.Name == s.spec.MCP.Name && p.Status == "failed" {
			s.emit(engine.Event{Kind: engine.Notice, Text: "slack tools MCP server failed to start: " + p.Error})
		}
	}
}

// onRequest answers approval requests. Sessions run unattended, so anything that needs a
// human is declined and the agent is told why (it can ask in the Slack thread instead).
func (s *session) onRequest(method string, params json.RawMessage) (any, error) {
	deny := func(resp any) (any, error) {
		s.emit(engine.Event{Kind: engine.Notice, Text: "declined " + method})
		return resp, nil
	}
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		return deny(map[string]any{"decision": "decline"})
	case "execCommandApproval", "applyPatchApproval":
		return deny(map[string]any{"decision": "denied"})
	case "item/permissions/requestApproval":
		return deny(map[string]any{"permissions": map[string]any{}, "scope": "turn"})
	case "mcpServer/elicitation/request":
		return deny(map[string]any{"action": "decline"})
	case "item/tool/requestUserInput":
		return deny(map[string]any{"answers": map[string]any{}})
	}
	return nil, fmt.Errorf("unsupported request %s", method)
}

// childEnv drops sandbox markers inherited when the daemon itself was started from a Codex
// shell, then applies engines.codex.env.
func childEnv(spec engine.Spec) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToUpper(k), "CODEX_SANDBOX") {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	return env
}
