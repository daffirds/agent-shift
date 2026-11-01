// Package session turns matching Slack messages into agent sessions and keeps each
// thread wired to its session: replies and reactions are steered in, the CLI process is
// stopped when idle and resumed on new activity, and state survives daemon restarts.
package session

import (
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/daffirds/agent-shift/internal/app"
	"github.com/daffirds/agent-shift/internal/config"
)

type State string

const (
	StateQueued   State = "queued"   // waiting for a free slot (max_sessions)
	StateRunning  State = "running"  // a turn is in progress
	StateIdle     State = "idle"     // process alive, waiting for the next message
	StateSleeping State = "sleeping" // process stopped; resumes on new thread activity
	StateEnded    State = "ended"    // no longer listening to the thread
	StateFailed   State = "failed"   // could not start; resumes on new thread activity
)

// Live reports whether the thread still steers the session.
func (s State) Live() bool { return s != StateEnded }

// Record is a session's persisted state: sessions/<id>/session.json.
type Record struct {
	ID          string       `json:"id"`
	Agent       string       `json:"agent"`
	AgentConfig config.Agent `json:"agent_config"` // snapshot, used if the agent is later removed
	Engine      string       `json:"engine"`
	EngineID    string       `json:"engine_session_id,omitempty"`
	TeamID      string       `json:"team_id"`
	Channel     string       `json:"channel"`
	ChannelName string       `json:"channel_name,omitempty"`
	ThreadTS    string       `json:"thread_ts"`
	Permalink   string       `json:"permalink,omitempty"`
	From        string       `json:"from,omitempty"`
	Title       string       `json:"title,omitempty"`
	Workdir     string       `json:"workdir"`
	Worktree    string       `json:"worktree,omitempty"`
	State       State        `json:"state"`
	LastError   string       `json:"last_error,omitempty"`
	Turns       int          `json:"turns"`
	CreatedAt   time.Time    `json:"created_at"`
	LastActive  time.Time    `json:"last_active"`
	// Posted lists "channel/ts" of messages this session sent (edits/deletes are limited to these).
	Posted []string `json:"posted,omitempty"`
	// Known lists ts of messages seen in the thread, to route reactions on them.
	Known []string `json:"known,omitempty"`
}

// Dir is the session's private directory.
func (r *Record) Dir() string { return filepath.Join(app.SessionsDir(), r.ID) }

// FilesDir receives files downloaded from Slack.
func (r *Record) FilesDir() string { return filepath.Join(r.Dir(), "files") }

// TranscriptPath is the raw engine protocol log.
func (r *Record) TranscriptPath() string { return filepath.Join(r.Dir(), "transcript.jsonl") }

func (r *Record) save() error {
	if err := os.MkdirAll(r.Dir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(r.Dir(), "session.json"), data, 0o600)
}

// LoadRecords reads every persisted session, newest first.
func LoadRecords() ([]*Record, error) {
	entries, err := os.ReadDir(app.SessionsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Record
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(app.SessionsDir(), e.Name(), "session.json"))
		if err != nil {
			continue
		}
		var r Record
		if json.Unmarshal(data, &r) == nil && r.ID != "" {
			out = append(out, &r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// FindRecord resolves a full or unambiguous prefix session ID.
func FindRecord(prefix string) (*Record, error) {
	recs, err := LoadRecords()
	if err != nil {
		return nil, err
	}
	var hit *Record
	for _, r := range recs {
		if r.ID == prefix {
			return r, nil
		}
		if strings.HasPrefix(r.ID, prefix) {
			if hit != nil {
				return nil, os.ErrExist
			}
			hit = r
		}
	}
	if hit == nil {
		return nil, os.ErrNotExist
	}
	return hit, nil
}

const idAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = idAlphabet[int(b[i])%len(idAlphabet)]
	}
	return string(b)
}
