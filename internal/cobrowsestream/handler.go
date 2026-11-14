package cobrowsestream

import (
	"bytes"
	"context"
	"encoding/json"
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
	var sawScreencastFrame atomic.Bool
	onFrame := func(f screencastFrame) {
		sawScreencastFrame.Store(true)
		holder.set(f.jpeg)
	}
	tsess, hasTargets := s.session.(TargetSession)
	var snap TargetSnapshot
	var cdp *cdpClient
	if hasTargets {
		snap, err = tsess.Targets(id)
		if err == nil {
			cdp, err = dialCDPURL(snap.ActiveWebSocketURL, onFrame, maxCDPMessageBytes)
		}
	} else {
		cdp, err = dialCDP(debugPort, onFrame)
	}
	if err != nil {
		s.logger.Printf("CDP attach failed for session=%s: %v", id, err)
		return
	}
	// The reader goroutine and the writer loop share the current CDP client;
	// a page switch replaces it. Order matters on exit: stopScreencast
	// (politeness) first, Close (the real no-leak guarantee) last.
	var current atomic.Pointer[cdpClient]
	current.Store(cdp)
	defer func() {
		if c := current.Load(); c != nil {
			c.stopScreencast()
			c.Close()
		}
	}()

	if err := cdp.startScreencast(s.cfg.Quality, s.cfg.MaxWidth, s.cfg.MaxHeight, s.cfg.EveryNth); err != nil {
		s.logger.Printf("startScreencast failed for session=%s: %v", id, err)
		return
	}

	uiMessages := make(chan map[string]any, 4)
	resync := make(chan struct{}, 1)
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
				if m, ok := parseInputMessage(data); ok && s.session.ViewerHasControl(id) {
					switch m.Type {
					case InputTypeSelectProbe, InputTypeSelect:
						result, err := current.Load().selectAt(m)
						if err != nil {
							s.logger.Printf("viewer select failed: %v", err)
						}
						if result == nil {
							result = map[string]any{"type": "select_error", "message": "No selectable control at this point"}
						}
						if m.Type == InputTypeSelectProbe {
							result["probe_id"] = m.ProbeID
						}
						select {
						case uiMessages <- result:
						case <-ctx.Done():
							return
						}
					case InputTypeTargetSwitch, InputTypeTargetClose:
						if !hasTargets {
							continue
						}
						var terr error
						if m.Type == InputTypeTargetSwitch {
							terr = tsess.SwitchTarget(id, m.Target)
						} else {
							terr = tsess.CloseTarget(id, m.Target)
						}
						if terr != nil {
							s.logger.Printf("viewer %s failed: %v", m.Type, terr)
						}
						select {
						case resync <- struct{}{}:
						default:
						}
					default:
						s.dispatchInput(current.Load(), m)
					}
				}
			}
		}
	}()

	// Single writer: init above, then frames + pings here. gorilla panics on
	// concurrent writes, so no other goroutine writes to this socket.
	ping := time.NewTicker(pingPeriod)
	defer ping.Stop()
	firstFrameFallback := time.NewTimer(750 * time.Millisecond)
	defer firstFrameFallback.Stop()
	var targetTick <-chan time.Time
	var lastTargets []byte
	activeID := snap.Active
	cdpDone := cdp.Done()
	if hasTargets {
		t := time.NewTicker(targetPollPeriod)
		defer t.Stop()
		targetTick = t.C
		if !s.sendTargets(conn, snap, &lastTargets) {
			return
		}
	}

	// syncTargets reads the pages, tells the viewer about changes, and moves
	// the screencast to a new active page. It returns false when the viewer or
	// the browser is gone.
	syncTargets := func() bool {
		next, err := tsess.Targets(id)
		if err != nil {
			return false
		}
		if !s.sendTargets(conn, next, &lastTargets) {
			return false
		}
		if next.Active != activeID || (isClosed(cdpDone) && next.ActiveWebSocketURL != "") {
			nc, err := dialCDPURL(next.ActiveWebSocketURL, onFrame, maxCDPMessageBytes)
			if err != nil {
				s.logger.Printf("CDP switch failed for session=%s: %v", id, err)
				return false
			}
			old := current.Swap(nc)
			if old != nil {
				old.stopScreencast()
				old.Close()
			}
			holder.take()
			sawScreencastFrame.Store(false)
			if err := nc.startScreencast(s.cfg.Quality, s.cfg.MaxWidth, s.cfg.MaxHeight, s.cfg.EveryNth); err != nil {
				s.logger.Printf("startScreencast failed for session=%s: %v", id, err)
				return false
			}
			activeID = next.Active
			cdpDone = nc.Done()
			firstFrameFallback.Reset(750 * time.Millisecond)
		}
		return true
	}

	for {
		select {
		case msg := <-uiMessages:
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteJSON(msg); err != nil {
				return
			}
		case <-ctx.Done():
			return
		case <-cdpDone:
			// The page went away. With target tracking it is usually a popup
			// that closed, so move to the opener; otherwise (or when the
			// browser itself is gone) tear the viewer down.
			if !hasTargets || !syncTargets() || isClosed(cdpDone) {
				return
			}
		case <-targetTick:
			if !syncTargets() {
				return
			}
		case <-resync:
			if !syncTargets() {
				return
			}
		case <-firstFrameFallback.C:
			if !sawScreencastFrame.Load() {
				// Chrome may not emit a screencast event for a static page. Ask
				// for its current image once, without blocking the stream writer.
				c := current.Load()
				go func() {
					captureCtx, stop := context.WithTimeout(ctx, 5*time.Second)
					defer stop()
					image, err := c.captureFirstFrame(captureCtx, s.cfg.Quality)
					if err == nil && !sawScreencastFrame.Load() && ctx.Err() == nil {
						holder.set(image)
					}
				}()
			}
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

// targetPollPeriod is how often the stream re-reads the session's pages, so a
// popup the page opens by itself reaches the viewer promptly.
const targetPollPeriod = 400 * time.Millisecond

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// sendTargets writes the targets TEXT frame when it differs from the last one
// sent. It returns false on a write failure.
func (s *Server) sendTargets(conn *websocket.Conn, snap TargetSnapshot, last *[]byte) bool {
	msg := TargetsMessage{Type: TargetsType, Active: snap.Active, Targets: snap.Targets}
	if msg.Targets == nil {
		msg.Targets = []TargetInfo{}
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return true
	}
	if bytes.Equal(b, *last) {
		return true
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		return false
	}
	*last = b
	return true
}

// dispatchInput routes one decoded viewer input message to CDP.
func (s *Server) dispatchInput(cdp *cdpClient, m InputMessage) {
	switch m.Type {
	case InputTypeMouse:
		_ = cdp.dispatchMouse(m)
	case InputTypeKey:
		_ = cdp.dispatchKey(m)
	case InputTypePaste:
		// The reader's live ViewerHasControl gate applies before this dispatch.
		// Insert literal text without consulting either browser's clipboard.
		_ = cdp.send("Input.insertText", map[string]any{"text": m.Text})
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
