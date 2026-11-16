package slack

// Event is a normalized inbound Slack event, produced by polling. Exactly one of Message/Reaction is set.
type Event struct {
	TeamID   string
	EventID  string
	Message  *Message
	Reaction *ReactionEvent
}

type ReactionEvent struct {
	Added    bool
	User     string
	Name     string
	Channel  string
	TS       string // the reacted-to message
	ItemUser string
	EventTS  string
}

// Relevant subtypes: a new message someone wrote (or a workflow/bot posted).
// Everything else (edits, deletes, joins, topic changes…) is ignored by the relay.
var newMessageSubtypes = map[string]bool{
	"":                 true,
	"bot_message":      true,
	"file_share":       true,
	"thread_broadcast": true,
	"me_message":       true,
}

// IsNewMessage reports whether this message event represents a freshly posted message.
func (m *Message) IsNewMessage() bool { return newMessageSubtypes[m.Subtype] && !m.Hidden }
