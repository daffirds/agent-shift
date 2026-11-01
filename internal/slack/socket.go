package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// EventCallback is the `events_api` payload delivered over Socket Mode.
type EventCallback struct {
	TeamID         string          `json:"team_id"`
	APIAppID       string          `json:"api_app_id"`
	Event          json.RawMessage `json:"event"`
	EventID        string          `json:"event_id"`
	EventTime      int64           `json:"event_time"`
	Authorizations []struct {
		TeamID string `json:"team_id"`
		UserID string `json:"user_id"`
		IsBot  bool   `json:"is_bot"`
	} `json:"authorizations"`
}

type envelope struct {
	Type           string          `json:"type"`
	EnvelopeID     string          `json:"envelope_id"`
	Payload        json.RawMessage `json:"payload"`
	Reason         string          `json:"reason"`
	NumConnections int             `json:"num_connections"`
	RetryAttempt   int             `json:"retry_attempt"`
	ConnectionInfo struct {
		AppID string `json:"app_id"`
	} `json:"connection_info"` // hello only
}

var errReconnect = errors.New("reconnecting")

// Socket maintains a Socket Mode connection and hands every Events API callback to OnEvent.
// Envelopes are acknowledged before OnEvent runs, so OnEvent must not block for long.
type Socket struct {
	App     *Client // client authenticated with the app-level token (xapp-…)
	Log     *slog.Logger
	OnEvent func(*EventCallback)
	// OnState is told about connection changes (optional). connected=true means Slack said hello.
	OnState func(connected bool, err error)

	mu    sync.Mutex
	drop  context.CancelCauseFunc // closes the current connection
	appID string
}

// AppID is the Slack app's ID, as announced by the last hello.
func (s *Socket) AppID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appID
}

// Reconnect drops the current connection; Run opens a fresh one right away.
func (s *Socket) Reconnect(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drop != nil {
		s.drop(fmt.Errorf("%w: %s", errReconnect, reason))
	}
}

// Run connects and reconnects until ctx is cancelled.
func (s *Socket) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		start := time.Now()
		err := s.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if s.OnState != nil {
			s.OnState(false, err)
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		if errors.Is(err, errReconnect) {
			s.Log.Debug("socket mode reconnect", "reason", err)
			continue
		}
		if IsCode(err, "invalid_auth", "not_authed", "token_revoked", "account_inactive") {
			backoff = 5 * time.Minute // nothing will fix itself quickly; don't hammer Slack
		}
		s.Log.Warn("socket mode disconnected; retrying", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}

func (s *Socket) session(ctx context.Context) error {
	wsURL, err := s.App.ConnectionsOpen(ctx)
	if err != nil {
		return err
	}
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(32 << 20)

	ctx, cancel := context.WithCancelCause(ctx)
	s.mu.Lock()
	s.drop = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.drop = nil
		s.mu.Unlock()
		cancel(nil)
	}()
	go func() { // keepalive: detect half-open connections
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, 20*time.Second)
				err := conn.Ping(pctx)
				pcancel()
				if err != nil {
					cancel(fmt.Errorf("ping: %w", err))
					return
				}
			}
		}
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
				return cause
			}
			return err
		}
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			s.Log.Warn("socket mode: bad frame", "err", err)
			continue
		}
		if env.EnvelopeID != "" {
			ack, _ := json.Marshal(map[string]string{"envelope_id": env.EnvelopeID})
			if err := conn.Write(ctx, websocket.MessageText, ack); err != nil {
				return fmt.Errorf("ack: %w", err)
			}
		}
		switch env.Type {
		case "hello":
			if id := env.ConnectionInfo.AppID; id != "" {
				s.mu.Lock()
				s.appID = id
				s.mu.Unlock()
			}
			if s.OnState != nil {
				s.OnState(true, nil)
			}
			if env.NumConnections > 1 {
				s.Log.Warn("another process is connected with the same Slack app; events will be split between them",
					"connections", env.NumConnections)
			}
		case "disconnect":
			return fmt.Errorf("%w: %s", errReconnect, env.Reason)
		case "events_api":
			var cb EventCallback
			if err := json.Unmarshal(env.Payload, &cb); err != nil {
				s.Log.Warn("socket mode: bad events_api payload", "err", err)
				continue
			}
			s.OnEvent(&cb)
		}
	}
}
