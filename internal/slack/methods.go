package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"strconv"
	"strings"
)

type cursorMeta struct {
	Metadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

// AuthInfo is the result of auth.test.
type AuthInfo struct {
	URL    string `json:"url"`
	Team   string `json:"team"`
	User   string `json:"user"`
	TeamID string `json:"team_id"`
	UserID string `json:"user_id"`
	BotID  string `json:"bot_id"`
	AppID  string `json:"app_id"`
}

func (c *Client) AuthTest(ctx context.Context) (*AuthInfo, error) {
	var out AuthInfo
	return &out, c.Call(ctx, "auth.test", nil, &out)
}

func (c *Client) TeamInfo(ctx context.Context) (*Team, error) {
	var out struct {
		Team Team `json:"team"`
	}
	return &out.Team, c.Call(ctx, "team.info", nil, &out)
}

// ConnectionsOpen returns a Socket Mode WebSocket URL. Requires an app-level token.
func (c *Client) ConnectionsOpen(ctx context.Context) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	return out.URL, c.Call(ctx, "apps.connections.open", nil, &out)
}

func (c *Client) ConversationInfo(ctx context.Context, id string) (*Conversation, error) {
	var out struct {
		Channel Conversation `json:"channel"`
	}
	err := c.Call(ctx, "conversations.info", url.Values{"channel": {id}, "include_num_members": {"true"}}, &out)
	return &out.Channel, err
}

// HistoryOptions page through conversations.history / conversations.replies.
type HistoryOptions struct {
	Limit     int
	Oldest    string
	Latest    string
	Inclusive bool
	Cursor    string
}

func (o HistoryOptions) values(v url.Values) url.Values {
	if o.Limit > 0 {
		v.Set("limit", strconv.Itoa(o.Limit))
	}
	if o.Oldest != "" {
		v.Set("oldest", o.Oldest)
	}
	if o.Latest != "" {
		v.Set("latest", o.Latest)
	}
	if o.Inclusive {
		v.Set("inclusive", "true")
	}
	if o.Cursor != "" {
		v.Set("cursor", o.Cursor)
	}
	return v
}

type MessagesPage struct {
	Messages []Message `json:"messages"`
	HasMore  bool      `json:"has_more"`
	cursorMeta
}

func (p *MessagesPage) NextCursor() string { return p.Metadata.NextCursor }

// History returns channel messages, newest first.
func (c *Client) History(ctx context.Context, channel string, o HistoryOptions) (*MessagesPage, error) {
	var out MessagesPage
	return &out, c.Call(ctx, "conversations.history", o.values(url.Values{"channel": {channel}}), &out)
}

// Replies returns a thread (parent first), oldest first.
func (c *Client) Replies(ctx context.Context, channel, ts string, o HistoryOptions) (*MessagesPage, error) {
	var out MessagesPage
	return &out, c.Call(ctx, "conversations.replies", o.values(url.Values{"channel": {channel}, "ts": {ts}}), &out)
}

// GetMessage fetches a single message, whether it is a parent or a thread reply.
func (c *Client) GetMessage(ctx context.Context, channel, ts string) (*Message, error) {
	page, err := c.History(ctx, channel, HistoryOptions{Latest: ts, Oldest: ts, Inclusive: true, Limit: 1})
	if err == nil {
		for i := range page.Messages {
			if page.Messages[i].TS == ts {
				return &page.Messages[i], nil
			}
		}
	}
	page, err = c.Replies(ctx, channel, ts, HistoryOptions{Latest: ts, Oldest: ts, Inclusive: true, Limit: 1})
	if err != nil {
		return nil, err
	}
	for i := range page.Messages {
		if page.Messages[i].TS == ts {
			return &page.Messages[i], nil
		}
	}
	return nil, &APIError{Method: "conversations.replies", Code: "message_not_found"}
}

// OpenConversation opens (or finds) a DM or group DM with the given users.
func (c *Client) OpenConversation(ctx context.Context, users ...string) (string, error) {
	var out struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	err := c.Call(ctx, "conversations.open", url.Values{"users": {strings.Join(users, ",")}}, &out)
	return out.Channel.ID, err
}

// UserConversations lists conversations the user is a member of.
func (c *Client) UserConversations(ctx context.Context, types, cursor string) ([]Conversation, string, error) {
	var out struct {
		Channels []Conversation `json:"channels"`
		cursorMeta
	}
	v := url.Values{"types": {types}, "exclude_archived": {"true"}, "limit": {"200"}}
	if cursor != "" {
		v.Set("cursor", cursor)
	}
	err := c.Call(ctx, "users.conversations", v, &out)
	return out.Channels, out.Metadata.NextCursor, err
}

func (c *Client) UserInfo(ctx context.Context, id string) (*User, error) {
	var out struct {
		User User `json:"user"`
	}
	return &out.User, c.Call(ctx, "users.info", url.Values{"user": {id}}, &out)
}

func (c *Client) LookupUserByEmail(ctx context.Context, email string) (*User, error) {
	var out struct {
		User User `json:"user"`
	}
	return &out.User, c.Call(ctx, "users.lookupByEmail", url.Values{"email": {email}}, &out)
}

func (c *Client) UsersList(ctx context.Context, cursor string) ([]User, string, error) {
	var out struct {
		Members []User `json:"members"`
		cursorMeta
	}
	v := url.Values{"limit": {"200"}}
	if cursor != "" {
		v.Set("cursor", cursor)
	}
	err := c.Call(ctx, "users.list", v, &out)
	return out.Members, out.Metadata.NextCursor, err
}

func (c *Client) BotInfo(ctx context.Context, id string) (*Bot, error) {
	var out struct {
		Bot Bot `json:"bot"`
	}
	return &out.Bot, c.Call(ctx, "bots.info", url.Values{"bot": {id}}, &out)
}

// PostParams is a chat.postMessage request.
type PostParams struct {
	Channel        string `json:"channel"`
	Text           string `json:"text,omitempty"`
	Blocks         []any  `json:"blocks,omitempty"`
	ThreadTS       string `json:"thread_ts,omitempty"`
	ReplyBroadcast bool   `json:"reply_broadcast,omitempty"`
	UnfurlLinks    *bool  `json:"unfurl_links,omitempty"`
	UnfurlMedia    *bool  `json:"unfurl_media,omitempty"`
}

type PostResult struct {
	Channel string  `json:"channel"`
	TS      string  `json:"ts"`
	Message Message `json:"message"`
}

func (c *Client) PostMessage(ctx context.Context, p PostParams) (*PostResult, error) {
	var out PostResult
	return &out, c.Call(ctx, "chat.postMessage", p, &out)
}

func (c *Client) UpdateMessage(ctx context.Context, channel, ts, text string, blocks []any) error {
	return c.Call(ctx, "chat.update", map[string]any{"channel": channel, "ts": ts, "text": text, "blocks": blocks}, nil)
}

func (c *Client) DeleteMessage(ctx context.Context, channel, ts string) error {
	return c.Call(ctx, "chat.delete", map[string]any{"channel": channel, "ts": ts}, nil)
}

func (c *Client) Permalink(ctx context.Context, channel, ts string) (string, error) {
	var out struct {
		Permalink string `json:"permalink"`
	}
	err := c.Call(ctx, "chat.getPermalink", url.Values{"channel": {channel}, "message_ts": {ts}}, &out)
	return out.Permalink, err
}

func (c *Client) AddReaction(ctx context.Context, channel, ts, name string) error {
	err := c.Call(ctx, "reactions.add", url.Values{"channel": {channel}, "timestamp": {ts}, "name": {strings.Trim(name, ":")}}, nil)
	if IsCode(err, "already_reacted") {
		return nil
	}
	return err
}

func (c *Client) RemoveReaction(ctx context.Context, channel, ts, name string) error {
	err := c.Call(ctx, "reactions.remove", url.Values{"channel": {channel}, "timestamp": {ts}, "name": {strings.Trim(name, ":")}}, nil)
	if IsCode(err, "no_reaction") {
		return nil
	}
	return err
}

func (c *Client) FileInfo(ctx context.Context, id string) (*File, error) {
	var out struct {
		File File `json:"file"`
	}
	return &out.File, c.Call(ctx, "files.info", url.Values{"file": {id}}, &out)
}

// UploadParams shares a file into a conversation (optionally a thread).
type UploadParams struct {
	Channel        string
	ThreadTS       string
	Filename       string
	Title          string
	InitialComment string
	Content        io.Reader
	Size           int64
	// OnFileID is called with the new file's ID before it is shared, so callers can
	// recognise the resulting message event (which may arrive before UploadFile returns).
	OnFileID func(id string)
}

// UploadFile uses the files.getUploadURLExternal → upload → files.completeUploadExternal flow.
func (c *Client) UploadFile(ctx context.Context, p UploadParams) (*File, error) {
	var ticket struct {
		UploadURL string `json:"upload_url"`
		FileID    string `json:"file_id"`
	}
	err := c.Call(ctx, "files.getUploadURLExternal", url.Values{
		"filename": {p.Filename},
		"length":   {strconv.FormatInt(p.Size, 10)},
	}, &ticket)
	if err != nil {
		return nil, err
	}
	if err := c.upload(ctx, ticket.UploadURL, p.Content, p.Size); err != nil {
		return nil, err
	}
	if p.OnFileID != nil {
		p.OnFileID(ticket.FileID)
	}
	title := p.Title
	if title == "" {
		title = p.Filename
	}
	files, _ := json.Marshal([]map[string]string{{"id": ticket.FileID, "title": title}})
	v := url.Values{"files": {string(files)}}
	if p.Channel != "" {
		v.Set("channel_id", p.Channel)
	}
	if p.ThreadTS != "" {
		v.Set("thread_ts", p.ThreadTS)
	}
	if p.InitialComment != "" {
		v.Set("initial_comment", p.InitialComment)
	}
	var out struct {
		Files []File `json:"files"`
	}
	if err := c.Call(ctx, "files.completeUploadExternal", v, &out); err != nil {
		return nil, err
	}
	if len(out.Files) == 0 {
		return &File{ID: ticket.FileID, Name: p.Filename}, nil
	}
	return &out.Files[0], nil
}

// SearchMatch is a search.messages hit.
type SearchMatch struct {
	TS       string `json:"ts"`
	Text     string `json:"text"`
	User     string `json:"user"`
	Username string `json:"username"`
	Channel  struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"channel"`
	Permalink string `json:"permalink"`
}

func (c *Client) SearchMessages(ctx context.Context, query string, count int) ([]SearchMatch, int, error) {
	var out struct {
		Messages struct {
			Total   int           `json:"total"`
			Matches []SearchMatch `json:"matches"`
		} `json:"messages"`
	}
	if count <= 0 {
		count = 20
	}
	err := c.Call(ctx, "search.messages", url.Values{
		"query": {query}, "count": {strconv.Itoa(count)}, "sort": {"timestamp"}, "sort_dir": {"desc"},
	}, &out)
	return out.Messages.Matches, out.Messages.Total, err
}
