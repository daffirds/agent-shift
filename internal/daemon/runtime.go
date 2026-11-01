package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/daffirds/agent-shift/internal/app"
	"github.com/daffirds/agent-shift/internal/config"
	"github.com/daffirds/agent-shift/internal/proc"
	"github.com/daffirds/agent-shift/internal/session"
)

// Runtime describes the running daemon; written to daemon.json for the CLI to find it.
type Runtime struct {
	PID     int       `json:"pid"`
	Addr    string    `json:"addr"`
	Token   string    `json:"token"`
	Started time.Time `json:"started"`
	Version string    `json:"version"`
}

var ErrNotRunning = errors.New("the agent-shift daemon is not running (start it with `agent-shift start`)")

func writeRuntime(r *Runtime) error {
	data, _ := json.MarshalIndent(r, "", "  ")
	return config.WriteFileAtomic(app.RuntimePath(), data, 0o600)
}

// ReadRuntime returns the live daemon's runtime info, or ErrNotRunning.
func ReadRuntime() (*Runtime, error) {
	data, err := os.ReadFile(app.RuntimePath())
	if err != nil {
		return nil, ErrNotRunning
	}
	var r Runtime
	if json.Unmarshal(data, &r) != nil || r.PID == 0 || !proc.Alive(r.PID) {
		return nil, ErrNotRunning
	}
	return &r, nil
}

// Client talks to the daemon's local control API.
type Client struct {
	rt   *Runtime
	http *http.Client
}

// Connect finds the running daemon.
func Connect() (*Client, error) {
	rt, err := ReadRuntime()
	if err != nil {
		return nil, err
	}
	return &Client{rt: rt, http: &http.Client{Timeout: 2 * time.Minute}}, nil
}

func (c *Client) Runtime() *Runtime { return c.rt }

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+c.rt.Addr+path, bytes.NewReader(nil))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.rt.Token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("daemon unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("daemon: http %d", resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

// Status is the daemon's health summary.
type Status struct {
	PID        int                   `json:"pid"`
	Version    string                `json:"version"`
	Started    time.Time             `json:"started"`
	Workspaces []WorkspaceStatus     `json:"workspaces"`
	Agents     []AgentStatus         `json:"agents"`
	Sessions   map[session.State]int `json:"sessions"`
}

type WorkspaceStatus struct {
	TeamID    string         `json:"team_id"`
	Name      string         `json:"name"`
	User      string         `json:"user"`
	Connected bool           `json:"connected"`
	Since     time.Time      `json:"since"`
	Error     string         `json:"error,omitempty"`
	Synced    time.Time      `json:"synced"`             // events received up to here
	CatchUp   *CatchUpStatus `json:"catch_up,omitempty"` // the last replay after downtime
}

type AgentStatus struct {
	Name     string `json:"name"`
	Engine   string `json:"engine"`
	Disabled bool   `json:"disabled"`
	Workdir  string `json:"workdir"`
}

func (c *Client) Status(ctx context.Context) (*Status, error) {
	var s Status
	return &s, c.do(ctx, http.MethodGet, "/api/status", &s)
}

func (c *Client) Sessions(ctx context.Context) ([]session.Record, error) {
	var out []session.Record
	return out, c.do(ctx, http.MethodGet, "/api/sessions", &out)
}

func (c *Client) StopSession(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/sessions/"+id+"/stop", nil)
}

func (c *Client) InterruptSession(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/sessions/"+id+"/interrupt", nil)
}

// ReloadResult reports what a reload changed.
type ReloadResult struct {
	Warnings []string `json:"warnings"`
}

func (c *Client) Reload(ctx context.Context) (*ReloadResult, error) {
	var r ReloadResult
	return &r, c.do(ctx, http.MethodPost, "/api/reload", &r)
}

func (c *Client) Shutdown(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/shutdown", nil)
}
