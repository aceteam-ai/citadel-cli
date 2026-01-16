// internal/terminal/protocol_test.go
package terminal

import (
	"encoding/json"
	"testing"
)

func TestMessageTypes(t *testing.T) {
	tests := []struct {
		name    string
		msgType string
	}{
		{"input", MessageTypeInput},
		{"output", MessageTypeOutput},
		{"resize", MessageTypeResize},
		{"error", MessageTypeError},
		{"ping", MessageTypePing},
		{"pong", MessageTypePong},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.msgType != tt.name {
				t.Errorf("message type %s != %s", tt.msgType, tt.name)
			}
		})
	}
}

func TestNewInputMessage(t *testing.T) {
	data := []byte("hello world")
	msg := NewInputMessage(data)

	if msg.Type != MessageTypeInput {
		t.Errorf("expected type %s, got %s", MessageTypeInput, msg.Type)
	}
	if string(msg.Payload) != "hello world" {
		t.Errorf("expected payload 'hello world', got '%s'", string(msg.Payload))
	}
}

func TestNewOutputMessage(t *testing.T) {
	data := []byte("terminal output")
	msg := NewOutputMessage(data)

	if msg.Type != MessageTypeOutput {
		t.Errorf("expected type %s, got %s", MessageTypeOutput, msg.Type)
	}
	if string(msg.Payload) != "terminal output" {
		t.Errorf("expected payload 'terminal output', got '%s'", string(msg.Payload))
	}
}

func TestNewResizeMessage(t *testing.T) {
	msg := NewResizeMessage(120, 40)

	if msg.Type != MessageTypeResize {
		t.Errorf("expected type %s, got %s", MessageTypeResize, msg.Type)
	}
	if msg.Cols != 120 {
		t.Errorf("expected cols 120, got %d", msg.Cols)
	}
	if msg.Rows != 40 {
		t.Errorf("expected rows 40, got %d", msg.Rows)
	}
}

func TestNewErrorMessage(t *testing.T) {
	msg := NewErrorMessage("something went wrong")

	if msg.Type != MessageTypeError {
		t.Errorf("expected type %s, got %s", MessageTypeError, msg.Type)
	}
	if msg.Error != "something went wrong" {
		t.Errorf("expected error 'something went wrong', got '%s'", msg.Error)
	}
}

func TestNewPingPongMessages(t *testing.T) {
	ping := NewPingMessage()
	if ping.Type != MessageTypePing {
		t.Errorf("expected type %s, got %s", MessageTypePing, ping.Type)
	}

	pong := NewPongMessage()
	if pong.Type != MessageTypePong {
		t.Errorf("expected type %s, got %s", MessageTypePong, pong.Type)
	}
}

func TestMessageMarshalUnmarshal(t *testing.T) {
	tests := []struct {
		name string
		msg  *Message
	}{
		{
			name: "input message",
			msg:  NewInputMessage([]byte("test input")),
		},
		{
			name: "output message",
			msg:  NewOutputMessage([]byte("test output")),
		},
		{
			name: "resize message",
			msg:  NewResizeMessage(80, 24),
		},
		{
			name: "error message",
			msg:  NewErrorMessage("test error"),
		},
		{
			name: "ping message",
			msg:  NewPingMessage(),
		},
		{
			name: "pong message",
			msg:  NewPongMessage(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Marshal
			data, err := tt.msg.Marshal()
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}

			// Unmarshal
			decoded, err := UnmarshalMessage(data)
			if err != nil {
				t.Fatalf("UnmarshalMessage() error = %v", err)
			}

			if decoded.Type != tt.msg.Type {
				t.Errorf("Type mismatch: got %s, want %s", decoded.Type, tt.msg.Type)
			}
			if string(decoded.Payload) != string(tt.msg.Payload) {
				t.Errorf("Payload mismatch: got %s, want %s", string(decoded.Payload), string(tt.msg.Payload))
			}
			if decoded.Cols != tt.msg.Cols {
				t.Errorf("Cols mismatch: got %d, want %d", decoded.Cols, tt.msg.Cols)
			}
			if decoded.Rows != tt.msg.Rows {
				t.Errorf("Rows mismatch: got %d, want %d", decoded.Rows, tt.msg.Rows)
			}
			if decoded.Error != tt.msg.Error {
				t.Errorf("Error mismatch: got %s, want %s", decoded.Error, tt.msg.Error)
			}
		})
	}
}

func TestUnmarshalMessageInvalid(t *testing.T) {
	_, err := UnmarshalMessage([]byte("not valid json"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestMessageValidate(t *testing.T) {
	tests := []struct {
		name    string
		msg     *Message
		wantErr error
	}{
		{
			name:    "valid input",
			msg:     NewInputMessage([]byte("test")),
			wantErr: nil,
		},
		{
			name:    "valid output",
			msg:     NewOutputMessage([]byte("test")),
			wantErr: nil,
		},
		{
			name:    "valid resize",
			msg:     NewResizeMessage(80, 24),
			wantErr: nil,
		},
		{
			name:    "invalid resize - zero cols",
			msg:     &Message{Type: MessageTypeResize, Cols: 0, Rows: 24},
			wantErr: ErrInvalidResize,
		},
		{
			name:    "invalid resize - zero rows",
			msg:     &Message{Type: MessageTypeResize, Cols: 80, Rows: 0},
			wantErr: ErrInvalidResize,
		},
		{
			name:    "valid error",
			msg:     NewErrorMessage("test"),
			wantErr: nil,
		},
		{
			name:    "valid ping",
			msg:     NewPingMessage(),
			wantErr: nil,
		},
		{
			name:    "valid pong",
			msg:     NewPongMessage(),
			wantErr: nil,
		},
		{
			name:    "invalid message type",
			msg:     &Message{Type: "unknown"},
			wantErr: ErrInvalidMessageType,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.msg.Validate()
			if err != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestMessageJSONFormat(t *testing.T) {
	// Test that the JSON format is correct
	msg := &Message{
		Type:    MessageTypeResize,
		Cols:    120,
		Rows:    40,
		Payload: []byte("test"),
		Error:   "error msg",
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Unmarshal to map error: %v", err)
	}

	// Check field names
	if _, ok := m["type"]; !ok {
		t.Error("expected 'type' field in JSON")
	}
	if _, ok := m["cols"]; !ok {
		t.Error("expected 'cols' field in JSON")
	}
	if _, ok := m["rows"]; !ok {
		t.Error("expected 'rows' field in JSON")
	}
	if _, ok := m["error"]; !ok {
		t.Error("expected 'error' field in JSON")
	}
}
