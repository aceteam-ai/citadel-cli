// Package chat provides node-to-node messaging over the AceTeam Redis API proxy.
//
// Messages are delivered in real-time via WebSocket-backed Pub/Sub and persisted
// to Redis Streams for history. Each organization gets a scoped set of channels.
package chat

import (
	"encoding/json"
	"fmt"
	"time"
)

// Message represents a chat message exchanged between nodes.
type Message struct {
	FromNodeID   string    `json:"from_node_id"`
	FromNodeName string    `json:"from_node_name"`
	Channel      string    `json:"channel"`
	Body         string    `json:"body"`
	Timestamp    time.Time `json:"ts"`
}

// PresenceInfo represents the online presence of a node.
type PresenceInfo struct {
	NodeID   string    `json:"node_id"`
	NodeName string    `json:"node_name"`
	LastSeen time.Time `json:"last_seen"`
}

// IsOnline returns true if the node was seen within the given timeout.
func (p PresenceInfo) IsOnline(timeout time.Duration) bool {
	return time.Since(p.LastSeen) < timeout
}

// ChannelName constructs the Pub/Sub channel name for a chat channel within an org.
func ChannelName(orgID, channel string) string {
	return fmt.Sprintf("chat:v1:org_%s:%s", orgID, channel)
}

// PresenceChannel constructs the Pub/Sub channel name for presence within an org.
func PresenceChannel(orgID string) string {
	return fmt.Sprintf("chat:v1:org_%s:presence", orgID)
}

// StreamName constructs the Redis Streams key for persistent message history.
func StreamName(orgID, channel string) string {
	return fmt.Sprintf("chat:v1:org_%s:%s:stream", orgID, channel)
}

// MarshalMessage serializes a Message to JSON bytes.
func MarshalMessage(msg Message) ([]byte, error) {
	return json.Marshal(msg)
}

// UnmarshalMessage deserializes a Message from JSON bytes.
func UnmarshalMessage(data []byte) (Message, error) {
	var msg Message
	err := json.Unmarshal(data, &msg)
	return msg, err
}

// UnmarshalMessageFromMap converts a map[string]any (from WebSocket) to a Message.
func UnmarshalMessageFromMap(m map[string]any) (Message, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return Message{}, fmt.Errorf("marshal map: %w", err)
	}
	return UnmarshalMessage(data)
}

// UnmarshalPresenceFromMap converts a map[string]any to PresenceInfo.
func UnmarshalPresenceFromMap(m map[string]any) (PresenceInfo, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return PresenceInfo{}, fmt.Errorf("marshal map: %w", err)
	}
	var p PresenceInfo
	err = json.Unmarshal(data, &p)
	return p, err
}
