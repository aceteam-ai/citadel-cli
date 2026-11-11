package cobrowsestream

import (
	"context"
	"net/http"
	"sync/atomic"
)

// StreamHandler is the stream route as an http.Handler, for mounting inside
// another, authenticated mux. Citadel local mode's loopback viewer
// (internal/localviewer) uses it behind its own Host, Origin, cookie and CSRF
// checks, so the unauthenticated mesh listener (Server.Start) never runs there.
// The wire contract, single-viewer claim and per-message input gate
// (ViewerHasControl) are exactly the Server's.
type StreamHandler struct{ s *Server }

// NewStreamHandler returns a mountable stream handler. checkOrigin decides
// every WebSocket upgrade; a nil checkOrigin refuses all of them.
func NewStreamHandler(cfg Config, session Session, checkOrigin func(*http.Request) bool) *StreamHandler {
	s := NewServer(cfg, session)
	if checkOrigin == nil {
		checkOrigin = func(*http.Request) bool { return false }
	}
	s.upgrader.CheckOrigin = checkOrigin
	return &StreamHandler{s: s}
}

// ServeHTTP serves one viewer. The session id is the "id" query parameter.
func (h *StreamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.s.handleStream(w, r) }

// SetSilent switches to a no-op logger.
func (h *StreamHandler) SetSilent() { h.s.SetSilent() }

// ActiveConnections reports live viewers.
func (h *StreamHandler) ActiveConnections() int64 { return atomic.LoadInt64(&h.s.activeConns) }

// CloseViewers cancels every live viewer so its CDP attachment is torn down.
func (h *StreamHandler) CloseViewers() {
	h.s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(h.s.active))
	for _, vc := range h.s.active {
		cancels = append(cancels, vc.cancel)
	}
	h.s.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}
