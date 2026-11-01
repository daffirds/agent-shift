package session

import (
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/daffirds/agent-shift/internal/config"
	"github.com/daffirds/agent-shift/internal/slack"
)

// systemUsers post notifications that should never start or steer a session.
var systemUsers = map[string]bool{"USLACKBOT": true, "USLACK": true}

var regexCache sync.Map // pattern → *regexp.Regexp

func compiled(pattern string) *regexp.Regexp {
	if v, ok := regexCache.Load(pattern); ok {
		return v.(*regexp.Regexp)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	regexCache.Store(pattern, re)
	return re
}

// matchTrigger reports whether a parent-level message starts a session for this trigger.
// text is the rendered message (text + blocks + attachments).
func matchTrigger(t *config.Trigger, ws *Workspace, m *slack.Message, text string) bool {
	if t.Workspace != ws.TeamID || !slices.ContainsFunc(t.Conversations, func(r config.Ref) bool { return r.ID == m.Channel }) {
		return false
	}
	if len(t.From) > 0 && !senderIn(t.From, ws, m) {
		return false
	}
	if len(t.Contains) > 0 {
		lt := strings.ToLower(text)
		if !slices.ContainsFunc(t.Contains, func(s string) bool { return strings.Contains(lt, strings.ToLower(s)) }) {
			return false
		}
	}
	if t.Regex != "" {
		re := compiled(t.Regex)
		if re == nil || !re.MatchString(text) {
			return false
		}
	}
	return true
}

// senderIn matches users ("me", U…), bots (B…) and apps/workflows (A…).
func senderIn(refs []config.Ref, ws *Workspace, m *slack.Message) bool {
	for _, r := range refs {
		switch id := r.ID; {
		case id == config.SelfRef:
			if m.User == ws.UserID {
				return true
			}
		case id == m.User && m.User != "", id == m.BotID && m.BotID != "", id == m.AppID && m.AppID != "":
			return true
		case m.BotProfile != nil && (id == m.BotProfile.ID || id == m.BotProfile.AppID):
			return true
		}
	}
	return false
}

func userIn(refs []config.Ref, ws *Workspace, user string) bool {
	return senderIn(refs, ws, &slack.Message{User: user})
}

// ttlSet remembers keys for a while (dedupe, own-post detection).
type ttlSet struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]time.Time
}

func newTTLSet(ttl time.Duration) *ttlSet { return &ttlSet{ttl: ttl, m: map[string]time.Time{}} }

func (s *ttlSet) add(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.m[key] = now
	if len(s.m) > 4096 {
		for k, t := range s.m {
			if now.Sub(t) > s.ttl {
				delete(s.m, k)
			}
		}
	}
}

func (s *ttlSet) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.m[key]
	return ok && time.Since(t) < s.ttl
}

// addNew records key and reports whether it was not seen before.
func (s *ttlSet) addNew(key string) bool {
	if s.has(key) {
		return false
	}
	s.add(key)
	return true
}

// limiter caps concurrently running CLI processes, first come first served.
type limiter struct {
	mu      sync.Mutex
	max     int
	used    int
	waiters []chan struct{}
}

func (l *limiter) setMax(n int) {
	l.mu.Lock()
	l.max = max(n, 1)
	l.wakeLocked()
	l.mu.Unlock()
}

func (l *limiter) wakeLocked() {
	for l.used < l.max && len(l.waiters) > 0 {
		ch := l.waiters[0]
		l.waiters = l.waiters[1:]
		l.used++
		close(ch)
	}
}

// tryAcquire takes a slot if one is free right now.
func (l *limiter) tryAcquire() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.used < l.max && len(l.waiters) == 0 {
		l.used++
		return true
	}
	return false
}

// wait returns a channel closed once a slot has been granted, and a cancel func that
// gives the slot back (or leaves the queue) if the caller stops waiting.
func (l *limiter) wait() (<-chan struct{}, func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ch := make(chan struct{})
	l.waiters = append(l.waiters, ch)
	l.wakeLocked()
	return ch, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		for i, w := range l.waiters {
			if w == ch {
				l.waiters = append(l.waiters[:i], l.waiters[i+1:]...)
				return
			}
		}
		// Already granted: hand it back.
		l.used--
		l.wakeLocked()
	}
}

func (l *limiter) release() {
	l.mu.Lock()
	l.used = max(l.used-1, 0)
	l.wakeLocked()
	l.mu.Unlock()
}
