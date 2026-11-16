package session

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/daffirds/agent-shift/internal/config"
	"github.com/daffirds/agent-shift/internal/slack"
)

// fakeSlack answers the read methods the poller uses and records every call.
type fakeSlack struct {
	mu      sync.Mutex
	calls   map[string][]url.Values
	history []slack.Message
	replies map[string][]slack.Message // thread ts → messages (parent first)
	search  []map[string]any
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	method := r.URL.Path[len("/api/"):]
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[method] = append(f.calls[method], form)
	out := map[string]any{"ok": true}
	switch method {
	case "conversations.history":
		var msgs []slack.Message
		for _, m := range f.history {
			if slack.TSTime(m.TS).After(slack.TSTime(form.Get("oldest"))) {
				msgs = append(msgs, m)
			}
		}
		out["messages"] = msgs
	case "conversations.replies":
		out["messages"] = f.replies[form.Get("ts")]
	case "search.messages":
		out["messages"] = map[string]any{"matches": f.search, "paging": map[string]int{"page": 1, "pages": 1}}
	default:
		out = map[string]any{"ok": false, "error": "unknown_method"}
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeSlack) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls[method])
}

func newTestPoller(t *testing.T, f *fakeSlack, agents []config.Agent) (*Poller, *Workspace) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("AGENT_SHIFT_HOME", t.TempDir())
	m := NewManager(Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	cfg := &config.Config{Agents: agents}
	cfg.Defaults()
	m.SetConfig(cfg)
	api := slack.NewClient("xoxp-test").WithBase(srv.URL + "/api/")
	ws := &Workspace{TeamID: "T1", Name: "test", UserID: "U1", UserName: "me", API: api, Dir: slack.NewDirectory(api)}
	m.SetWorkspace(ws)
	return m.NewPoller("T1", time.Now().Add(-time.Hour)), ws
}

func ts(t time.Time) string { return slack.FormatTS(t) }

func TestPollerReadsWatchedConversationOnABudget(t *testing.T) {
	f := &fakeSlack{calls: map[string][]url.Values{}, history: []slack.Message{{Type: "message", User: "U2", Text: "hi", TS: ts(time.Now().Add(-time.Minute))}}}
	agent := config.Agent{Name: "a", Engine: "claude", Workdir: "/tmp", Trigger: config.Trigger{Workspace: "T1", Conversations: []config.Ref{{ID: "C1"}}, Contains: []string{"never matches"}}}
	p, _ := newTestPoller(t, f, []config.Agent{agent})
	ctx := context.Background()

	p.Tick(ctx)
	if n := f.count("conversations.history"); n != 1 {
		t.Fatalf("history calls after first tick = %d, want 1", n)
	}
	first := f.calls["conversations.history"][0].Get("oldest")
	if !slack.TSTime(first).Before(time.Now().Add(-50 * time.Minute)) {
		t.Fatalf("first read should start at the watermark, oldest=%s", first)
	}
	// Ticks inside convEvery must not read again.
	p.Tick(ctx)
	p.Tick(ctx)
	if n := f.count("conversations.history"); n != 1 {
		t.Fatalf("history calls within %s = %d, want 1", convEvery, n)
	}
	// The next read starts after the newest message seen.
	p.convs["C1"].next = time.Time{}
	p.Tick(ctx)
	if got, want := f.calls["conversations.history"][1].Get("oldest"), f.history[0].TS; got != want {
		t.Fatalf("second read oldest = %s, want %s", got, want)
	}
	// No tracked threads: no search at all.
	if n := f.count("search.messages"); n != 0 {
		t.Fatalf("search calls = %d, want 0 without cold threads", n)
	}
	if st := p.Status(); st.Conversations != 1 || st.Error != "" || st.Covered.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("status = %+v", st)
	}
}

func TestSweepWakesOnlyThreadsWithNewReplies(t *testing.T) {
	now := time.Now()
	parentA, parentB := ts(now.Add(-48*time.Hour)), ts(now.Add(-47*time.Hour))
	reply := ts(now.Add(-30 * time.Second))
	f := &fakeSlack{calls: map[string][]url.Values{}, search: []map[string]any{{
		"ts": reply, "channel": map[string]string{"id": "C1"},
		"permalink": "https://x.slack.com/archives/C1/p1?thread_ts=" + parentA + "&cid=C1",
	}}}
	p, ws := newTestPoller(t, f, nil)
	a := &threadCursor{channel: "C1", ts: parentA, covered: p.since}
	b := &threadCursor{channel: "C1", ts: parentB, covered: p.since}
	p.threads["C1/"+parentA], p.threads["C1/"+parentB] = a, b

	if err := p.sweep(context.Background(), ws, now, now.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !now.Before(a.hotUntil) {
		t.Fatal("thread with a new reply should be hot")
	}
	if now.Before(b.hotUntil) {
		t.Fatal("thread without replies should stay cold")
	}
	q := f.calls["search.messages"][0]
	if q.Get("sort") != "timestamp" || q.Get("sort_dir") != "desc" || q.Get("query") == "" {
		t.Fatalf("search params = %v", q)
	}
	// a and b were never read directly, but search vouches for them up to now-searchLag.
	if got := p.covered(now); got.Before(now.Add(-searchLag - time.Second)) {
		t.Fatalf("covered = %s, want about now-%s", got, searchLag)
	}
}

func TestPollThreadDiffsReactions(t *testing.T) {
	now := time.Now()
	parent := ts(now.Add(-time.Hour))
	f := &fakeSlack{calls: map[string][]url.Values{}, replies: map[string][]slack.Message{
		parent: {{Type: "message", User: "U1", TS: parent, ThreadTS: parent, Reactions: []slack.Reaction{{Name: "eyes", Users: []string{"U1"}}}}},
	}}
	p, ws := newTestPoller(t, f, nil)
	th := &threadCursor{channel: "C1", ts: parent, latest: parent, covered: p.since}
	if err := p.pollThread(context.Background(), ws, th, now.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !th.baseline || !th.reacts[parent]["eyes U1"] || now.Before(th.hotUntil) {
		t.Fatalf("first read is a baseline only: %+v", th)
	}
	f.mu.Lock()
	f.replies[parent][0].Reactions = append(f.replies[parent][0].Reactions, slack.Reaction{Name: "white_check_mark", Users: []string{"U2"}})
	f.replies[parent] = append(f.replies[parent], slack.Message{Type: "message", User: "U2", Text: "done?", TS: ts(now), ThreadTS: parent})
	f.mu.Unlock()
	if err := p.pollThread(context.Background(), ws, th, now.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !th.reacts[parent]["white_check_mark U2"] || th.latest != ts(now) || !time.Now().Before(th.hotUntil) {
		t.Fatalf("second read should see the reaction and reply and turn hot: %+v", th)
	}
}

func TestSearchMatchThreadTS(t *testing.T) {
	m := slack.SearchMatch{TS: "2.0", Permalink: "https://x.slack.com/archives/C1/p2?thread_ts=1.0&cid=C1"}
	if m.ThreadTS() != "1.0" {
		t.Fatal(m.ThreadTS())
	}
	m = slack.SearchMatch{TS: "1.0", Permalink: "https://x.slack.com/archives/C1/p1"}
	if m.ThreadTS() != "" {
		t.Fatal(m.ThreadTS())
	}
}

func TestBucket(t *testing.T) {
	now := time.Now()
	b := newBucket(30, now) // 0.5/s, holds 5
	n := 0
	for b.take(now) {
		n++
	}
	if n != 5 {
		t.Fatalf("burst = %d, want 5", n)
	}
	if b.take(now.Add(time.Second)) {
		t.Fatal("half a token after 1s")
	}
	if !b.take(now.Add(2 * time.Second)) {
		t.Fatal("a token after 2s")
	}
}
