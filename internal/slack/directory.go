package slack

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Directory caches users, bots and conversations so rendering and routing stay cheap.
type Directory struct {
	api *Client
	ttl time.Duration

	mu    sync.Mutex
	users map[string]entry[*User]
	bots  map[string]entry[*Bot]
	convs map[string]entry[*Conversation]

	allUsers listCache[User]
	myConvs  listCache[Conversation]
}

// API exposes the underlying client.
func (d *Directory) API() *Client { return d.api }

type entry[T any] struct {
	v   T
	err error
	at  time.Time
}

func NewDirectory(api *Client) *Directory {
	return &Directory{
		api: api, ttl: 6 * time.Hour,
		users: map[string]entry[*User]{},
		bots:  map[string]entry[*Bot]{},
		convs: map[string]entry[*Conversation]{},
	}
}

func cached[T any](d *Directory, m map[string]entry[T], id string, fetch func() (T, error)) (T, error) {
	d.mu.Lock()
	e, ok := m[id]
	d.mu.Unlock()
	if ok && time.Since(e.at) < d.ttl && (e.err == nil || time.Since(e.at) < time.Minute) {
		return e.v, e.err
	}
	v, err := fetch()
	d.mu.Lock()
	m[id] = entry[T]{v: v, err: err, at: time.Now()}
	d.mu.Unlock()
	return v, err
}

func (d *Directory) User(ctx context.Context, id string) (*User, error) {
	return cached(d, d.users, id, func() (*User, error) { return d.api.UserInfo(ctx, id) })
}

func (d *Directory) Bot(ctx context.Context, id string) (*Bot, error) {
	return cached(d, d.bots, id, func() (*Bot, error) { return d.api.BotInfo(ctx, id) })
}

func (d *Directory) Conversation(ctx context.Context, id string) (*Conversation, error) {
	return cached(d, d.convs, id, func() (*Conversation, error) { return d.api.ConversationInfo(ctx, id) })
}

// UserName returns the display name of a user (or bot user), falling back to the ID.
func (d *Directory) UserName(ctx context.Context, id string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if strings.HasPrefix(id, "B") {
		if b, err := d.Bot(ctx, id); err == nil && b.Name != "" {
			return b.Name
		}
		return id
	}
	if u, err := d.User(ctx, id); err == nil {
		return u.Display()
	}
	return id
}

// ConversationName returns "#name" for channels, "DM with X" or "group DM" otherwise.
func (d *Directory) ConversationName(ctx context.Context, id string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := d.Conversation(ctx, id)
	if err != nil {
		return id
	}
	return d.Label(ctx, c)
}

// Label describes a conversation for humans.
func (d *Directory) Label(ctx context.Context, c *Conversation) string {
	switch {
	case c.IsIM:
		return "DM with " + d.UserName(ctx, c.User)
	case c.IsMPIM:
		return "group DM " + strings.TrimPrefix(c.Name, "mpdm-")
	default:
		return "#" + c.Name
	}
}

// SenderName names whoever posted the message (user, bot or workflow).
func (d *Directory) SenderName(ctx context.Context, m *Message) string {
	if m.User != "" {
		return d.UserName(ctx, m.User)
	}
	if m.BotProfile != nil && m.BotProfile.Name != "" {
		return m.BotProfile.Name
	}
	if m.Username != "" {
		return m.Username
	}
	if m.BotID != "" {
		return d.UserName(ctx, m.BotID)
	}
	return "unknown"
}
