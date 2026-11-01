package slack

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Message is a Slack message as found in events and in conversations.history/replies.
type Message struct {
	Type         string            `json:"type,omitempty"`
	Subtype      string            `json:"subtype,omitempty"`
	Channel      string            `json:"channel,omitempty"`
	ChannelType  string            `json:"channel_type,omitempty"`
	Team         string            `json:"team,omitempty"`
	User         string            `json:"user,omitempty"`
	BotID        string            `json:"bot_id,omitempty"`
	AppID        string            `json:"app_id,omitempty"`
	Username     string            `json:"username,omitempty"`
	BotProfile   *BotProfile       `json:"bot_profile,omitempty"`
	Text         string            `json:"text,omitempty"`
	TS           string            `json:"ts,omitempty"`
	ThreadTS     string            `json:"thread_ts,omitempty"`
	ParentUserID string            `json:"parent_user_id,omitempty"`
	ReplyCount   int               `json:"reply_count,omitempty"`
	Hidden       bool              `json:"hidden,omitempty"`
	Edited       *Edited           `json:"edited,omitempty"`
	Files        []File            `json:"files,omitempty"`
	Blocks       []json.RawMessage `json:"blocks,omitempty"`
	Attachments  []Attachment      `json:"attachments,omitempty"`
	Reactions    []Reaction        `json:"reactions,omitempty"`
	// Set on message_changed / message_deleted events.
	Message         *Message `json:"message,omitempty"`
	PreviousMessage *Message `json:"previous_message,omitempty"`
	DeletedTS       string   `json:"deleted_ts,omitempty"`
}

type Edited struct {
	User string `json:"user"`
	TS   string `json:"ts"`
}

type BotProfile struct {
	ID      string `json:"id"`
	AppID   string `json:"app_id"`
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
}

type Reaction struct {
	Name  string   `json:"name"`
	Count int      `json:"count"`
	Users []string `json:"users"`
}

type Attachment struct {
	Fallback   string            `json:"fallback,omitempty"`
	Color      string            `json:"color,omitempty"`
	Pretext    string            `json:"pretext,omitempty"`
	AuthorName string            `json:"author_name,omitempty"`
	Title      string            `json:"title,omitempty"`
	TitleLink  string            `json:"title_link,omitempty"`
	Text       string            `json:"text,omitempty"`
	Fields     []AttachmentField `json:"fields,omitempty"`
	Footer     string            `json:"footer,omitempty"`
	FromURL    string            `json:"from_url,omitempty"`
	Blocks     []json.RawMessage `json:"blocks,omitempty"`
	// Message unfurls / shares.
	IsMsgUnfurl bool   `json:"is_msg_unfurl,omitempty"`
	ChannelName string `json:"channel_name,omitempty"`
}

type AttachmentField struct {
	Title string `json:"title"`
	Value string `json:"value"`
}

type File struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Title              string `json:"title"`
	Mimetype           string `json:"mimetype"`
	Filetype           string `json:"filetype"`
	Size               int64  `json:"size"`
	URLPrivate         string `json:"url_private"`
	URLPrivateDownload string `json:"url_private_download"`
	Permalink          string `json:"permalink"`
	Mode               string `json:"mode"`
	FileAccess         string `json:"file_access"`
}

type User struct {
	ID       string `json:"id"`
	TeamID   string `json:"team_id"`
	Name     string `json:"name"`
	RealName string `json:"real_name"`
	Deleted  bool   `json:"deleted"`
	IsBot    bool   `json:"is_bot"`
	TZ       string `json:"tz"`
	Profile  struct {
		DisplayName string `json:"display_name"`
		RealName    string `json:"real_name"`
		Email       string `json:"email"`
		Title       string `json:"title"`
		BotID       string `json:"bot_id"`
	} `json:"profile"`
}

// Display is the name Slack shows for the user.
func (u *User) Display() string {
	for _, s := range []string{u.Profile.DisplayName, u.Profile.RealName, u.RealName, u.Name} {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return u.ID
}

type Conversation struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	IsChannel  bool   `json:"is_channel"`
	IsGroup    bool   `json:"is_group"`
	IsIM       bool   `json:"is_im"`
	IsMPIM     bool   `json:"is_mpim"`
	IsPrivate  bool   `json:"is_private"`
	IsArchived bool   `json:"is_archived"`
	IsMember   bool   `json:"is_member"`
	User       string `json:"user"` // the other party of a DM
	Updated    int64  `json:"updated"`
	Topic      struct {
		Value string `json:"value"`
	} `json:"topic"`
	Purpose struct {
		Value string `json:"value"`
	} `json:"purpose"`
	NumMembers int `json:"num_members"`
}

// Kind is "channel", "private channel", "dm" or "group dm".
func (c *Conversation) Kind() string {
	switch {
	case c.IsIM:
		return "dm"
	case c.IsMPIM:
		return "group dm"
	case c.IsPrivate || c.IsGroup:
		return "private channel"
	default:
		return "channel"
	}
}

type Bot struct {
	ID      string `json:"id"`
	AppID   string `json:"app_id"`
	UserID  string `json:"user_id"`
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
}

type Team struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Domain string `json:"domain"`
	URL    string `json:"url"`
}

// TSTime converts a message timestamp ("1730450000.123456") to a time; zero if malformed.
func TSTime(ts string) time.Time {
	sec, frac, _ := strings.Cut(ts, ".")
	s, err := strconv.ParseInt(sec, 10, 64)
	if err != nil || s <= 0 {
		return time.Time{}
	}
	us, _ := strconv.ParseInt((frac + "000000")[:6], 10, 64)
	return time.Unix(s, us*1000)
}

// FormatTS converts a time to a message timestamp, e.g. for history bounds.
func FormatTS(t time.Time) string { return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000) }

// IsThreadReply reports whether the message lives inside a thread (not the parent).
func (m *Message) IsThreadReply() bool { return m.ThreadTS != "" && m.ThreadTS != m.TS }

// SenderID is the user ID, or the bot ID for bot/workflow messages.
func (m *Message) SenderID() string {
	if m.User != "" {
		return m.User
	}
	if m.BotID != "" {
		return m.BotID
	}
	if m.BotProfile != nil {
		return m.BotProfile.ID
	}
	return ""
}

// HasBlockID reports whether any top-level block carries the given block_id.
func (m *Message) HasBlockID(id string) bool {
	for _, raw := range m.Blocks {
		var b struct {
			BlockID string `json:"block_id"`
		}
		if json.Unmarshal(raw, &b) == nil && b.BlockID == id {
			return true
		}
	}
	return false
}
