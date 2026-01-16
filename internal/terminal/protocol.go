// internal/terminal/protocol.go
package terminal

import "encoding/json"

// MessageType constants define the types of WebSocket messages
const (
	// MessageTypeInput is sent from client to server with terminal input
	MessageTypeInput = "input"

	// MessageTypeOutput is sent from server to client with terminal output
	MessageTypeOutput = "output"

	// MessageTypeResize is sent from client to server to resize the terminal
	MessageTypeResize = "resize"

	// MessageTypeError is sent from server to client to indicate an error
	MessageTypeError = "error"

	// MessageTypePing is sent from either side to check connection health
	MessageTypePing = "ping"

	// MessageTypePong is sent in response to a ping message
	MessageTypePong = "pong"
)

// Message represents a WebSocket message for terminal communication
type Message struct {
	// Type indicates the message type (input, output, resize, error, ping, pong)
	Type string `json:"type"`

	// Payload contains the data for input/output messages (base64-encoded for binary safety)
	Payload []byte `json:"payload,omitempty"`

	// Cols is the number of columns for resize messages
	Cols uint16 `json:"cols,omitempty"`

	// Rows is the number of rows for resize messages
	Rows uint16 `json:"rows,omitempty"`

	// Error contains the error message for error type messages
	Error string `json:"error,omitempty"`
}

// NewInputMessage creates a new input message
func NewInputMessage(data []byte) *Message {
	return &Message{
		Type:    MessageTypeInput,
		Payload: data,
	}
}

// NewOutputMessage creates a new output message
func NewOutputMessage(data []byte) *Message {
	return &Message{
		Type:    MessageTypeOutput,
		Payload: data,
	}
}

// NewResizeMessage creates a new resize message
func NewResizeMessage(cols, rows uint16) *Message {
	return &Message{
		Type: MessageTypeResize,
		Cols: cols,
		Rows: rows,
	}
}

// NewErrorMessage creates a new error message
func NewErrorMessage(err string) *Message {
	return &Message{
		Type:  MessageTypeError,
		Error: err,
	}
}

// NewPingMessage creates a new ping message
func NewPingMessage() *Message {
	return &Message{
		Type: MessageTypePing,
	}
}

// NewPongMessage creates a new pong message
func NewPongMessage() *Message {
	return &Message{
		Type: MessageTypePong,
	}
}

// Marshal serializes the message to JSON
func (m *Message) Marshal() ([]byte, error) {
	return json.Marshal(m)
}

// UnmarshalMessage deserializes a JSON message
func UnmarshalMessage(data []byte) (*Message, error) {
	var msg Message
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// Validate checks that the message is well-formed
func (m *Message) Validate() error {
	switch m.Type {
	case MessageTypeInput:
		// Input messages should have payload
		return nil
	case MessageTypeOutput:
		// Output messages should have payload
		return nil
	case MessageTypeResize:
		if m.Cols == 0 || m.Rows == 0 {
			return ErrInvalidResize
		}
		return nil
	case MessageTypeError:
		return nil
	case MessageTypePing, MessageTypePong:
		return nil
	default:
		return ErrInvalidMessageType
	}
}
