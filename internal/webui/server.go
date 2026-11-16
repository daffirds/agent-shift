// Package webui serves agent-shift's configuration UI: a small Vite app (embedded from dist/)
// plus a JSON API on 127.0.0.1. Every API call needs the random token printed in the link, and
// requests from other hosts or origins are refused, so other sites in the browser can't use it.
package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/daffirds/agent-shift/internal/ui"
)

//go:embed all:dist
var dist embed.FS

// Options configure Serve.
type Options struct {
	Addr        string // host:port; the host must be a loopback address
	OpenBrowser bool
}

type server struct {
	token string
	base  string // http://host:port

	mu     sync.Mutex
	models map[string]modelCache // engine → cached model list
}

// Serve runs the UI until ctx is cancelled (Ctrl+C).
func Serve(ctx context.Context, o Options) error {
	host, _, err := net.SplitHostPort(o.Addr)
	if err != nil {
		return fmt.Errorf("--addr: %w", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("--addr must be a loopback address (127.0.0.1 or localhost)")
	}
	ln, err := net.Listen("tcp", o.Addr)
	if err != nil {
		// The default port is taken (another `agent-shift ui`?): use any free one.
		if ln, err = net.Listen("tcp", net.JoinHostPort(host, "0")); err != nil {
			return err
		}
	}
	s := &server{token: randomHex(24), base: "http://" + ln.Addr().String(), models: map[string]modelCache{}}

	mux := http.NewServeMux()
	s.routes(mux)
	sub, _ := fs.Sub(dist, "dist")
	files := http.FileServerFS(sub)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if _, err := fs.Stat(sub, strings.TrimPrefix(r.URL.Path, "/")); r.URL.Path != "/" && err != nil {
			r.URL.Path = "/" // client-side routes (e.g. /connected) get the app
		}
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: s.guard(mux), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	link := s.base + "/#t=" + s.token
	ui.Banner("agent-shift ui", "configuration in your browser")
	ui.Info("Open %s", ui.Bold(link))
	ui.Info("%s", ui.Dim("Press Ctrl+C to stop. The link only works while this runs."))
	if o.OpenBrowser {
		_ = ui.OpenBrowser(link)
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// guard refuses requests whose Host or Origin isn't this server (DNS rebinding, cross-site
// requests) and API calls without the token.
func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.Host)
		if host == "" {
			host = r.Host
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
				writeErr(w, http.StatusForbidden, errors.New("cross-origin request refused"))
				return
			}
			got := r.Header.Get("X-Agent-Shift-Token")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
				writeErr(w, http.StatusUnauthorized, errors.New("open the link printed by `agent-shift ui`"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("bad request body: %w", err)
	}
	return nil
}
