package slack

import (
	"encoding/json"
	"fmt"
)

// Event is a normalized inbound Events API event. Exactly one of Message/Reaction is set.
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

// ParseEvent decodes the inner event of a callback. Unsupported event types return (nil, nil).
func ParseEvent(cb *EventCallback) (*Event, error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(cb.Event, &head); err != nil {
		return nil, err
	}
	ev := &Event{TeamID: cb.TeamID, EventID: cb.EventID}
	switch head.Type {
	case "message":
		var m Message
		if err := json.Unmarshal(cb.Event, &m); err != nil {
			return nil, fmt.Errorf("message event: %w", err)
		}
		ev.Message = &m
	case "reaction_added", "reaction_removed":
		var r struct {
			User     string `json:"user"`
			Reaction string `json:"reaction"`
			ItemUser string `json:"item_user"`
			EventTS  string `json:"event_ts"`
			Item     struct {
				Type    string `json:"type"`
				Channel string `json:"channel"`
				TS      string `json:"ts"`
			} `json:"item"`
		}
		if err := json.Unmarshal(cb.Event, &r); err != nil {
			return nil, fmt.Errorf("reaction event: %w", err)
		}
		if r.Item.Type != "message" {
			return nil, nil
		}
		ev.Reaction = &ReactionEvent{
			Added: head.Type == "reaction_added", User: r.User, Name: r.Reaction,
			Channel: r.Item.Channel, TS: r.Item.TS, ItemUser: r.ItemUser, EventTS: r.EventTS,
		}
	default:
		return nil, nil
	}
	return ev, nil
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
