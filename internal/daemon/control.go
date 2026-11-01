package daemon

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/daffirds/agent-shift/internal/app"
	"github.com/daffirds/agent-shift/internal/session"
)

// routes registers the local control API used by the CLI (status, sessions, reload, stop).
func (d *daemon) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/status", d.auth(d.handleStatus))
	mux.HandleFunc("GET /api/sessions", d.auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, d.mgr.Snapshot())
	}))
	mux.HandleFunc("POST /api/sessions/{id}/stop", d.auth(func(w http.ResponseWriter, r *http.Request) {
		d.respond(w, d.mgr.Stop(r.PathValue("id")))
	}))
	mux.HandleFunc("POST /api/sessions/{id}/interrupt", d.auth(func(w http.ResponseWriter, r *http.Request) {
		d.respond(w, d.mgr.Interrupt(r.PathValue("id")))
	}))
	mux.HandleFunc("POST /api/reload", d.auth(func(w http.ResponseWriter, r *http.Request) {
		res, err := d.reload()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, res)
	}))
	mux.HandleFunc("POST /api/shutdown", d.auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		go func() { time.Sleep(100 * time.Millisecond); d.stop() }()
	}))
}

func (d *daemon) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte("Bearer "+d.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		h(w, r)
	}
}

func (d *daemon) respond(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case errors.Is(err, session.ErrNoSession):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
}

func (d *daemon) handleStatus(w http.ResponseWriter, _ *http.Request) {
	cfg := d.mgr.Config()
	st := Status{PID: os.Getpid(), Version: app.Version, Started: d.started, Sessions: d.mgr.Counts()}
	d.mu.Lock()
	for _, wcfg := range cfg.Workspaces {
		ws := WorkspaceStatus{TeamID: wcfg.TeamID, Name: wcfg.Name, User: wcfg.UserName}
		if c := d.conns[wcfg.TeamID]; c != nil {
			ws.Connected = c.connected.Load()
			ws.Since, _ = c.since.Load().(time.Time)
			ws.Error, _ = c.lastErr.Load().(string)
			ws.Synced = d.marks.get(wcfg.TeamID)
			ws.CatchUp = c.catchUp.Load()
		} else {
			ws.Error = "no tokens; run `agent-shift slack connect`"
		}
		st.Workspaces = append(st.Workspaces, ws)
	}
	d.mu.Unlock()
	for _, a := range cfg.Agents {
		st.Agents = append(st.Agents, AgentStatus{Name: a.Name, Engine: a.Engine, Disabled: a.Disabled, Workdir: a.Workdir})
	}
	writeJSON(w, http.StatusOK, st)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
