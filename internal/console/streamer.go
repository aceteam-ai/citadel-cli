// internal/console/streamer.go
package console

import (
	"log"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/terminal"
	"github.com/gorilla/websocket"
)

// Streamer fans PTY output to multiple WebSocket viewers.
// It speaks the same JSON protocol as the terminal server
// (terminal.Message with type "output"), so the web Console tab
// can connect without any protocol changes.
type Streamer struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]struct{}
	silent  bool
}

// NewStreamer creates a new Streamer with no connected clients.
func NewStreamer() *Streamer {
	return &Streamer{
		clients: make(map[*websocket.Conn]struct{}),
	}
}

// SetSilent suppresses log output (useful when running inside the TUI).
func (s *Streamer) SetSilent(silent bool) {
	s.mu.Lock()
	s.silent = silent
	s.mu.Unlock()
}

// AddClient registers a WebSocket connection for receiving PTY output.
func (s *Streamer) AddClient(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[conn] = struct{}{}
	if !s.silent {
		log.Printf("[console/streamer] client added (%d total)", len(s.clients))
	}
}

// RemoveClient unregisters a WebSocket connection and closes it.
func (s *Streamer) RemoveClient(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, conn)
	_ = conn.Close()
	if !s.silent {
		log.Printf("[console/streamer] client removed (%d remaining)", len(s.clients))
	}
}

// ClientCount returns the number of connected viewers.
func (s *Streamer) ClientCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.clients)
}

// Broadcast sends PTY output bytes to all connected clients as
// terminal.MessageTypeOutput messages. Clients that fail to receive
// are removed.
func (s *Streamer) Broadcast(data []byte) {
	if len(data) == 0 {
		return
	}

	msg := terminal.NewOutputMessage(data)
	encoded, err := msg.Marshal()
	if err != nil {
		return
	}

	s.mu.RLock()
	clients := make([]*websocket.Conn, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.RUnlock()

	if len(clients) == 0 {
		return
	}

	var failed []*websocket.Conn
	for _, c := range clients {
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := c.WriteMessage(websocket.TextMessage, encoded); err != nil {
			failed = append(failed, c)
		}
	}

	for _, c := range failed {
		s.RemoveClient(c)
	}
}

// CloseAll disconnects all clients.
func (s *Streamer) CloseAll() {
	s.mu.Lock()
	clients := make([]*websocket.Conn, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.clients = make(map[*websocket.Conn]struct{})
	s.mu.Unlock()

	for _, c := range clients {
		_ = c.WriteMessage(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "session ended"),
		)
		_ = c.Close()
	}
}
