package daemon

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"sync"
	"time"

	"github.com/daffirds/agent-shift/internal/app"
	"github.com/daffirds/agent-shift/internal/config"
)

// Catch-up after downtime. Each workspace keeps a watermark (state.json): the time up to which
// its poller has read Slack. A new poller (daemon start, token change) starts there, so what
// was posted while the daemon was stopped is replayed; a wake from sleep needs nothing special,
// since the running poller's cursors simply haven't moved.

const (
	heartbeatEvery = 30 * time.Second
	// catchUpMargin re-reads a little before the watermark, in case Slack's clock differs from ours.
	catchUpMargin = 2 * time.Minute
)

// syncState is state.json. Per workspace: until when Slack events were received (the
// watermark). Per agent: since when it has been enabled, so a catch-up only replays what the
// agent would have seen live; one added while the daemon was stopped starts fresh.
type syncState struct {
	mu     sync.Mutex
	path   string
	synced map[string]time.Time // team ID → watermark
	agents map[string]time.Time // agent name → enabled since
}

type stateFile struct {
	Synced map[string]time.Time `json:"synced"`
	Agents map[string]time.Time `json:"agents"`
}

func loadState() *syncState {
	s := &syncState{path: app.StatePath(), synced: map[string]time.Time{}, agents: map[string]time.Time{}}
	var f stateFile
	if data, err := os.ReadFile(s.path); err == nil && json.Unmarshal(data, &f) == nil {
		if f.Synced != nil {
			s.synced = f.Synced
		}
		if f.Agents != nil {
			s.agents = f.Agents
		}
	}
	return s
}

func (s *syncState) get(team string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.synced[team]
}

func (s *syncState) set(team string, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.synced[team] = t.Round(0)
	return s.saveLocked()
}

func (s *syncState) forget(team string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.synced[team]; ok {
		delete(s.synced, team)
		_ = s.saveLocked()
	}
}

// trackAgents notes when each enabled agent was first seen, and forgets disabled or removed ones.
func (s *syncState) trackAgents(cfg *config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Round(0)
	enabled := map[string]bool{}
	changed := false
	for _, a := range cfg.Agents {
		if a.Disabled {
			continue
		}
		enabled[a.Name] = true
		if _, ok := s.agents[a.Name]; !ok {
			s.agents[a.Name] = now
			changed = true
		}
	}
	for name := range s.agents {
		if !enabled[name] {
			delete(s.agents, name)
			changed = true
		}
	}
	if changed {
		_ = s.saveLocked()
	}
}

func (s *syncState) agentsSince() map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.agents)
}

func (s *syncState) saveLocked() error {
	data, _ := json.MarshalIndent(stateFile{Synced: s.synced, Agents: s.agents}, "", "  ")
	return config.WriteFileAtomic(s.path, data, 0o600)
}

// heartbeat saves each workspace's watermark (what its poller has read up to), so the next
// start replays from there.
func (d *daemon) heartbeat(ctx context.Context) {
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		d.saveMarks()
	}
}

func (d *daemon) saveMarks() {
	d.mu.Lock()
	conns := make([]*connection, 0, len(d.conns))
	for _, c := range d.conns {
		conns = append(conns, c)
	}
	d.mu.Unlock()
	for _, c := range conns {
		if p := c.poller.Load(); p != nil {
			if cov := p.Covered(); !cov.IsZero() && cov.After(d.marks.get(c.ws.TeamID)) {
				if err := d.marks.set(c.ws.TeamID, cov); err != nil {
					d.log.Debug("cannot save state", "err", err)
				}
			}
		}
	}
}
