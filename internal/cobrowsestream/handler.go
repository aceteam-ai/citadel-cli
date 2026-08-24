package cobrowsestream

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// frameHolder is a single-slot, coalescing frame buffer: it keeps only the
// LATEST screencast frame plus a one-deep signal. A viewer that cannot keep up
// therefore drops intermediate frames instead of growing an unbounded queue --
// memory stays bounded to one frame no matter how far behind the viewer falls.
type frameHolder struct {
	mu     sync.Mutex
	latest []byte
	signal chan struct{}
}

func newFrameHolder() *frameHolder {
	return &frameHolder{signal: make(chan struct{}, 1)}
}

// set stores b as the latest frame (overwriting any unsent one) and wakes the
// writer. It never blocks, so the CDP read loop is never stalled by a slow viewer.
func (h *frameHolder) set(b []byte) {
	h.mu.Lock()
	h.latest = b
	h.mu.Unlock()
	select {
	case h.signal <- struct{}{}:
	default:
	}
}

// take returns and clears the latest frame, or nil if none is pending.
func (h *frameHolder) take() []byte {
	h.mu.Lock()
	b := h.latest
	h.latest = nil
	h.mu.Unlock()
	return b
}

// handleStream serves one viewer: it attaches CDP screencast to the addressed
// session, streams JPEG frames to the viewer, and forwards the viewer's input
// back to the session. Every exit path tears the CDP attachment down (defer
// Close), flips the session out of "attached" (defer MarkDetached), and frees the
// single-viewer claim (defer release) -- so no CDP attachment and no stuck
// "attached" state can leak.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Printf("websocket upgrade failed for %s: %v", r.RemoteAddr, err)
		return
	}
	defer conn.Close()

	if id == "" {
		s.rejectViewer(conn, "missing session id")
		return
	}

	debugPort, ok := s.session.AttachTarget(id)
	if !ok {
		s.rejectViewer(conn, "session not found or not attachable")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Single-viewer gate: refuse a second viewer so input is never ambiguous.
	// Claim BEFORE MarkAttached so a refused second viewer never touches state.
	vc, claimed := s.claim(id, cancel)
	if !claimed {
		s.rejectViewer(conn, "session already has a viewer")
		return
	}
	defer s.release(id, vc)

	s.session.MarkAttached(id)
	defer s.session.MarkDetached(id)

	atomic.AddInt64(&s.totalConns, 1)
	atomic.AddInt64(&s.activeConns, 1)
	s.logger.Printf("viewer connected: session=%s remote=%s", id, r.RemoteAddr)
	defer func() {
		atomic.AddInt64(&s.activeConns, -1)
		s.logger.Printf("viewer disconnected: session=%s remote=%s", id, r.RemoteAddr)
	}()

	// Wire contract step 1: one TEXT init frame before any BINARY frame.
	initBytes, err := NewInitMessage(id).Marshal()
	if err != nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	if err := conn.WriteMessage(websocket.TextMessage, initBytes); err != nil {
		return
	}

	holder := newFrameHolder()
	cdp, err := dialCDP(debugPort, func(f screencastFrame) { holder.set(f.jpeg) })
	if err != nil {
		s.logger.Printf("CDP attach failed for session=%s: %v", id, err)
		return
	}
	// Order matters: stopScreencast (politeness) runs first, Close (the real
	// no-leak guarantee) runs last. Defers execute LIFO.
	defer cdp.Close()
	defer cdp.stopScreencast()

	if err := cdp.startScreencast(s.cfg.Quality, s.cfg.MaxWidth, s.cfg.MaxHeight, s.cfg.EveryNth); err != nil {
		s.logger.Printf("startScreencast failed for session=%s: %v", id, err)
		return
	}

	// Reader goroutine: viewer input -> CDP. Also detects viewer disconnect and
	// keeps the read deadline fresh via pong, so a dead viewer flips the session
	// out of "attached" within pongWait. It never writes to the viewer socket.
	go func() {
		conn.SetReadLimit(viewerReadLimit)
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(pongWait))
		})
		for {
			mt, data, rerr := conn.ReadMessage()
			if rerr != nil {
				cancel()
				return
			}
			if mt == websocket.TextMessage {
				if m, ok := parseInputMessage(data); ok {
					s.dispatchInput(cdp, m)
				}
			}
		}
	}()

	// Single writer: init above, then frames + pings here. gorilla panics on
	// concurrent writes, so no other goroutine writes to this socket.
	ping := time.NewTicker(pingPeriod)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-cdp.Done():
			// Browser/session went away: tear the viewer down.
			return
		case <-holder.signal:
			frame := holder.take()
			if frame == nil {
				continue
			}
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
				return
			}
		case <-ping.C:
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// dispatchInput routes one decoded viewer input message to CDP.
func (s *Server) dispatchInput(cdp *cdpClient, m InputMessage) {
	switch m.Type {
	case InputTypeMouse:
		_ = cdp.dispatchMouse(m)
	case InputTypeKey:
		_ = cdp.dispatchKey(m)
	}
}

// rejectViewer best-effort tells the viewer why it was refused, then lets the
// deferred Close drop the socket. The close code is a normal policy violation so
// a viewer can distinguish "no such session" from a transport error.
func (s *Server) rejectViewer(conn *websocket.Conn, reason string) {
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	msg := websocket.FormatCloseMessage(websocket.ClosePolicyViolation, reason)
	_ = conn.WriteMessage(websocket.CloseMessage, msg)
}
