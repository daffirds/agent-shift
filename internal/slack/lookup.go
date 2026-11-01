package slack

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	conversationIDPattern = regexp.MustCompile(`^[CGD][A-Z0-9]{6,}$`)
	userIDPattern         = regexp.MustCompile(`^[UW][A-Z0-9]{6,}$`)
)

func IsConversationID(s string) bool { return conversationIDPattern.MatchString(s) }
func IsUserID(s string) bool         { return userIDPattern.MatchString(s) }

type listCache[T any] struct {
	mu    sync.Mutex
	items []T
	at    time.Time
}

func (c *listCache[T]) get(ttl time.Duration, fetch func() ([]T, error)) ([]T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items != nil && time.Since(c.at) < ttl {
		return c.items, nil
	}
	items, err := fetch()
	if err != nil {
		if c.items != nil {
			return c.items, nil // stale beats nothing
		}
		return nil, err
	}
	c.items, c.at = items, time.Now()
	return items, nil
}

// AllUsers lists every (non-deleted) member of the workspace, cached for an hour.
func (d *Directory) AllUsers(ctx context.Context) ([]User, error) {
	return d.allUsers.get(time.Hour, func() ([]User, error) {
		var all []User
		cursor := ""
		for {
			page, next, err := d.api.UsersList(ctx, cursor)
			if err != nil {
				return nil, err
			}
			for _, u := range page {
				if !u.Deleted {
					all = append(all, u)
				}
			}
			if next == "" {
				return all, nil
			}
			cursor = next
		}
	})
}

// MyConversations lists channels, DMs and group DMs the user belongs to, cached for 10 minutes.
func (d *Directory) MyConversations(ctx context.Context) ([]Conversation, error) {
	return d.myConvs.get(10*time.Minute, func() ([]Conversation, error) {
		var all []Conversation
		cursor := ""
		for {
			page, next, err := d.api.UserConversations(ctx, "public_channel,private_channel,mpim,im", cursor)
			if err != nil {
				return nil, err
			}
			all = append(all, page...)
			if next == "" {
				return all, nil
			}
			cursor = next
		}
	})
}

// FindUser resolves an ID, email, @handle, display name or real name to a user.
func (d *Directory) FindUser(ctx context.Context, query string) (*User, error) {
	q := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(query), "@"))
	q = strings.TrimSuffix(strings.TrimPrefix(q, "<@"), ">")
	if IsUserID(q) {
		return d.User(ctx, q)
	}
	if strings.Contains(q, "@") && strings.Contains(q, ".") {
		return d.api.LookupUserByEmail(ctx, q)
	}
	users, err := d.AllUsers(ctx)
	if err != nil {
		return nil, err
	}
	lq := strings.ToLower(q)
	var partial []*User
	for i := range users {
		u := &users[i]
		for _, n := range []string{u.Name, u.Profile.DisplayName, u.Profile.RealName, u.RealName} {
			ln := strings.ToLower(n)
			if ln == "" {
				continue
			}
			if ln == lq {
				return u, nil
			}
			if strings.Contains(ln, lq) {
				partial = append(partial, u)
				break
			}
		}
	}
	switch len(partial) {
	case 0:
		return nil, fmt.Errorf("no user matches %q", query)
	case 1:
		return partial[0], nil
	default:
		var names []string
		for _, u := range partial[:min(len(partial), 8)] {
			names = append(names, fmt.Sprintf("%s (%s)", u.Display(), u.ID))
		}
		return nil, fmt.Errorf("%q is ambiguous: %s", query, strings.Join(names, ", "))
	}
}

// ResolveConversation turns "#name", "C123", "@user", "U123", "email" or "U1,U2" into a
// conversation ID, opening a DM / group DM when needed.
func (d *Directory) ResolveConversation(ctx context.Context, target string) (string, error) {
	t := strings.TrimSpace(target)
	if t == "" {
		return "", fmt.Errorf("empty conversation")
	}
	if strings.HasPrefix(t, "<#") { // <#C123|name>
		t = strings.TrimSuffix(strings.TrimPrefix(t, "<#"), ">")
		t, _, _ = strings.Cut(t, "|")
	}
	if IsConversationID(t) {
		return t, nil
	}
	if strings.Contains(t, ",") {
		var ids []string
		for _, part := range strings.Split(t, ",") {
			u, err := d.FindUser(ctx, part)
			if err != nil {
				return "", err
			}
			ids = append(ids, u.ID)
		}
		return d.api.OpenConversation(ctx, ids...)
	}
	if strings.HasPrefix(t, "#") || !(strings.HasPrefix(t, "@") || IsUserID(t) || strings.Contains(t, "@")) {
		name := strings.ToLower(strings.TrimPrefix(t, "#"))
		convs, err := d.MyConversations(ctx)
		if err != nil {
			return "", err
		}
		for _, c := range convs {
			if strings.ToLower(c.Name) == name {
				return c.ID, nil
			}
		}
		if strings.HasPrefix(t, "#") {
			return "", fmt.Errorf("you are not a member of a channel named %s", t)
		}
	}
	u, err := d.FindUser(ctx, t)
	if err != nil {
		return "", err
	}
	return d.api.OpenConversation(ctx, u.ID)
}
