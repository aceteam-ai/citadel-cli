// internal/terminal/wsconn.go
package terminal

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// wsWriteTimeout bounds a single WriteMessage, mirroring the write-mutex
// discipline internal/redisapi/websocket.go established for the same bug
// class (citadel #720/#725). Serializing writes behind one mutex means a
// blocked write no longer stalls only its own caller, it stalls every writer
// queued behind the mutex, so the write must be bounded.
//
// 15s matches the http.Server WriteTimeout already set in Start (see
// tmuxInstallTimeout's comment in tmux.go for the same reference point): a
// wedged client socket surfaces as a write error well inside that window
// instead of leaving a goroutine parked indefinitely, and it is far above any
// plausible time to hand a terminal frame to the kernel on a healthy
// connection.
const wsWriteTimeout = 15 * time.Second

// safeConn serializes every gorilla "write method" (WriteMessage,
// SetWriteDeadline) on a single *websocket.Conn behind one mutex (citadel
// #729, same bug class as #720/PR #725).
//
// gorilla/websocket permits exactly one concurrent caller of a write method
// and panics with "concurrent write to websocket connection" otherwise
// (doc.go). A terminal session genuinely has more than one goroutine that
// can want the socket at once: handleConnection's PTY->WebSocket relay
// goroutine writes PTY output while the WebSocket->PTY main loop (the same
// function, different goroutine) writes error/resize-error/pong replies. Both
// call sites, plus the two early-return error writes in handleWebSocket
// before handleConnection is ever reached, share this one instance per
// connection so nothing writes to conn outside the lock.
//
// WriteControl (used by the ping handler in handleConnection) is
// deliberately NOT routed through here: gorilla documents it as safe to call
// concurrently with all other methods except possibly Close, and it does not
// consult the write deadline WriteMessage/SetWriteDeadline touch, so it needs
// no guard (verified against the pinned module source, same as the #725
// finding for the analogous ping/pong path in redisapi).
type safeConn struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

// newSafeConn wraps conn. One safeConn must be shared by every write site for
// the lifetime of that connection — constructing a second instance over the
// same conn would defeat the serialization, since two distinct mutexes don't
// exclude each other.
func newSafeConn(conn *websocket.Conn) *safeConn {
	return &safeConn{conn: conn}
}

// WriteMessage serializes conn.WriteMessage behind mu, with the deadline set
// and cleared inside the SAME critical section. SetWriteDeadline is itself
// one of gorilla's write methods (a bare field assignment on the Conn), so
// touching it outside the lock would race an in-flight WriteMessage from
// another goroutine — exactly the second bug the #725 investigation found in
// redisapi's Close.
func (s *safeConn) WriteMessage(messageType int, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	err := s.conn.WriteMessage(messageType, data)
	_ = s.conn.SetWriteDeadline(time.Time{})
	return err
}
