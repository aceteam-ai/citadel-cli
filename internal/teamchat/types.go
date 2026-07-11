// Package teamchat provides a typed HTTP client for the AceTeam Team Chat
// REST API — the v1 `/api/channels/**` surface backed by the `channels`,
// `channel_messages`, and `channel_members` tables. This is the same contract
// the iOS and Android apps consume (see aceteam `types/channels.ts`), so the
// TUI renders the same org conversation users see on every other platform.
//
// Live updates use light polling, matching the mobile clients: the platform's
// v1 chat WebSocket has been removed pending the v2 chat cutover, so polling
// is the current cross-client parity mechanism (aceteam-ai/citadel-cli#495).
package teamchat

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Timestamp wraps time.Time with tolerant JSON parsing. Supabase serializes
// TIMESTAMPTZ with an offset (e.g. "2026-07-01T12:00:00+00:00"), sometimes
// with fractional seconds, and the contract also admits offset-less local
// timestamps. A strict time.RFC3339 parse would reject some of these shapes.
type Timestamp struct {
	time.Time
}

// timestampLayouts are tried in order when parsing a Timestamp. Offset-less
// values are interpreted as UTC (Supabase stores TIMESTAMPTZ in UTC).
var timestampLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
}

// UnmarshalJSON implements json.Unmarshaler.
func (t *Timestamp) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("timestamp: %w", err)
	}
	if s == "" {
		t.Time = time.Time{}
		return nil
	}
	for _, layout := range timestampLayouts {
		if parsed, err := time.Parse(layout, s); err == nil {
			// Offset-less layouts parse into time.UTC already; keep as-is.
			t.Time = parsed
			return nil
		}
	}
	return fmt.Errorf("timestamp: unrecognized format %q", s)
}

// MarshalJSON implements json.Marshaler.
func (t Timestamp) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte(`""`), nil
	}
	return json.Marshal(t.Format(time.RFC3339))
}

// Channel is a team-chat channel row (aceteam zChannelSchema subset).
type Channel struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Description    *string `json:"description,omitempty"`
	Visibility     string  `json:"visibility"`
	OrganizationID string  `json:"organization_id"`
	CreatedBy      string  `json:"created_by"`
	CreatedAt      *string `json:"created_at,omitempty"`
	ArchivedAt     *string `json:"archived_at,omitempty"`
}

// Sender is the hydrated user join on a message (null for agent messages).
type Sender struct {
	ID       string  `json:"id"`
	Email    string  `json:"email"`
	FullName *string `json:"full_name,omitempty"`
}

// AgentSummary is the hydrated agent join on a message (null for human messages).
type AgentSummary struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Attachment is a file attached to a message.
type Attachment struct {
	ID       string `json:"id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	ByteSize int64  `json:"byte_size"`
}

// Message is a team-chat message (aceteam zChannelMessageWithSenderSchema
// subset — only the fields the TUI renders).
type Message struct {
	ID              string        `json:"id"`
	ChannelID       string        `json:"channel_id"`
	Content         string        `json:"content"`
	CreatedAt       Timestamp     `json:"created_at"`
	MessageType     string        `json:"message_type"`
	AgentID         *string       `json:"agent_id,omitempty"`
	ParentMessageID *string       `json:"parent_message_id,omitempty"`
	ReplyCount      int           `json:"reply_count"`
	Sender          *Sender       `json:"sender,omitempty"`
	Agent           *AgentSummary `json:"agent,omitempty"`
	Attachments     []Attachment  `json:"attachments,omitempty"`
}

// SenderLabel returns the best human-readable label for the message author:
// agent title for agent messages, then the sender's full name, then email,
// then a generic fallback.
func (m Message) SenderLabel() string {
	if m.MessageType == "agent" {
		if m.Agent != nil && m.Agent.Title != "" {
			return m.Agent.Title
		}
		return "agent"
	}
	if m.Sender != nil {
		if m.Sender.FullName != nil && strings.TrimSpace(*m.Sender.FullName) != "" {
			return strings.TrimSpace(*m.Sender.FullName)
		}
		if m.Sender.Email != "" {
			return m.Sender.Email
		}
	}
	return "unknown"
}

// MessagesPage is the paginated response from GET .../messages
// (aceteam zPaginatedMessagesResponseSchema). Messages are in chronological
// (oldest-first) order for the default "before" direction.
type MessagesPage struct {
	Messages   []Message `json:"messages"`
	HasMore    bool      `json:"hasMore"`
	NextCursor *string   `json:"nextCursor,omitempty"`
}

// Member is a channel member with the hydrated user join
// (aceteam zChannelMemberWithUserSchema subset).
type Member struct {
	ID     string  `json:"id"`
	UserID string  `json:"user_id"`
	Role   string  `json:"role"`
	Source string  `json:"source,omitempty"`
	User   *Sender `json:"user,omitempty"`
}

// Label returns the member's display label (name, then email, then user id).
func (m Member) Label() string {
	if m.User != nil {
		if m.User.FullName != nil && strings.TrimSpace(*m.User.FullName) != "" {
			return strings.TrimSpace(*m.User.FullName)
		}
		if m.User.Email != "" {
			return m.User.Email
		}
	}
	return m.UserID
}
