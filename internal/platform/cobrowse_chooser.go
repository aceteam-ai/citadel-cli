// File-chooser interception for co-browse sessions (citadel-cli#1721).
//
// A page that opens a file chooser in a session without a rendered dialog gets
// nothing: neither a person nor a script can answer it. The watcher keeps one
// extra CDP connection to the session's page with
// Page.setInterceptFileChooserDialog on, records the chooser a page opens
// (Page.fileChooserOpened) in the session status, and clears it when the
// input is filled through set_file_input or the page navigates. It never
// fills anything itself: choosing a file is the caller's decision.
package platform

import (
	"context"
	"encoding/json"
	"time"

	"github.com/gorilla/websocket"
)

// FileChooserPending is a file chooser a page opened and nobody has answered.
type FileChooserPending struct {
	// BackendNodeID names the input; pass it to set_file_input as backend_node_id.
	BackendNodeID int `json:"backend_node_id"`
	// Multiple is true when the input accepts more than one file.
	Multiple bool `json:"multiple"`
	// FrameID is the frame whose input opened the chooser.
	FrameID string `json:"frame_id,omitempty"`
	// OpenedAt is when the node saw the chooser open (RFC 3339, UTC).
	OpenedAt string `json:"opened_at,omitempty"`
	// FrameURL is the owning frame's URL as the browser reported it
	// (Page.getFrameTree, url plus urlFragment) shortly after the chooser
	// opened. Empty until that read returns. The human attach path compares it
	// with the frame's URL just before dispatch. Never serialized.
	FrameURL string `json:"-"`
}

// chooserMaxMessageBytes bounds one CDP message read by the watcher.
const chooserMaxMessageBytes = 1 << 20

// browserAlive reports whether this session still owns a running browser on
// port.
func (s *cobrowseSession) browserAlive(port int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc == nil || s.proc.debugPort != port {
		return false
	}
	select {
	case <-s.proc.exited:
		return false
	default:
		return true
	}
}

// chooserReconnectDelay is the pause before the watcher redials a page after
// its connection drops; a variable so tests need not sleep.
var chooserReconnectDelay = time.Second

// handleChooserEvent applies one CDP event to the pending chooser. It is the
// whole state machine, split out so tests can drive it without a browser.
// It returns the chooser a fileChooserOpened event recorded (nil otherwise), so
// the watcher can ask the browser for that chooser's frame URL.
func (s *cobrowseSession) handleChooserEvent(method string, params map[string]any) *FileChooserPending {
	switch method {
	case "Page.fileChooserOpened":
		id, ok := params["backendNodeId"].(float64)
		if !ok || id <= 0 {
			return nil
		}
		mode, _ := params["mode"].(string)
		frame, _ := params["frameId"].(string)
		p := &FileChooserPending{
			FrameID:       frame,
			BackendNodeID: int(id),
			Multiple:      mode == "selectMultiple",
			OpenedAt:      time.Now().UTC().Format(time.RFC3339),
		}
		s.mu.Lock()
		s.chooser = p
		s.mu.Unlock()
		return p
	case "Page.frameNavigated":
		// Only a navigation of the main frame (it has no parentId) clears it.
		if fr, ok := params["frame"].(map[string]any); ok {
			if _, sub := fr["parentId"]; !sub {
				s.clearChooser()
			}
		}
	}
	return nil
}

// setChooserFrameURL records the browser-reported frame URL on p, but only
// while p is still the pending chooser: a newer chooser or a clear wins.
func (s *cobrowseSession) setChooserFrameURL(p *FileChooserPending, url string) {
	s.mu.Lock()
	if s.chooser == p {
		p.FrameURL = url
	}
	s.mu.Unlock()
}

// chooserTreeIDBase keeps the watcher's getFrameTree request ids apart from
// its two setup calls.
const chooserTreeIDBase = 100

func (s *cobrowseSession) clearChooser() {
	s.mu.Lock()
	s.chooser = nil
	s.mu.Unlock()
}

// clearChooserFor clears the pending chooser when it is for the node just
// filled. A different node leaves it pending.
func (s *cobrowseSession) clearChooserFor(backendNodeID int) {
	s.mu.Lock()
	if s.chooser != nil && backendNodeID != 0 && s.chooser.BackendNodeID == backendNodeID {
		s.chooser = nil
	}
	s.mu.Unlock()
}

// startChooserWatch starts the watcher for a node-wide session. Local mode
// (page tracking) is not watched.
func (s *cobrowseSession) startChooserWatch(port int) {
	if s.tracking || port == 0 {
		return
	}
	s.mu.Lock()
	if s.watchStop != nil || s.watchStopped {
		s.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	s.watchStop = stop
	s.mu.Unlock()
	go s.watchChooser(port, stop)
}

func (s *cobrowseSession) stopChooserWatch() {
	// Mark first, so a start that arrives after any stop is a no-op even when
	// the once below has already been used.
	s.mu.Lock()
	s.watchStopped = true
	s.mu.Unlock()
	s.watchOnce.Do(func() {
		s.mu.Lock()
		stop := s.watchStop
		s.watchStop = nil
		s.chooser = nil
		s.mu.Unlock()
		if stop != nil {
			close(stop)
		}
	})
}

func (s *cobrowseSession) watchChooser(port int, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		if !s.browserAlive(port) {
			return
		}
		s.watchChooserOnce(port, stop)
		select {
		case <-stop:
			return
		case <-time.After(chooserReconnectDelay):
		}
	}
}

// watchChooserOnce dials the page once and reads events until the connection
// drops or the session stops.
func (s *cobrowseSession) watchChooserOnce(port int, stop <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	// Only ever attach to THIS session's browser: a dead port may be reused
	// by a later session's browser.
	s.mu.Lock()
	want := ""
	if s.proc != nil {
		want = s.proc.browserID
	}
	s.mu.Unlock()
	// No attach until the browser's id is known: the id is recorded at launch,
	// and without it nothing proves this port still answers for this session.
	if want == "" {
		return
	}
	if err := verifyCDPBrowserID(port, want); err != nil {
		return
	}
	dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
	target, err := pickTargetContext(dctx, port)
	if err != nil {
		dcancel()
		return
	}
	ws, _, err := websocket.DefaultDialer.DialContext(dctx, target.WebSocketDebuggerURL, nil)
	dcancel()
	if err != nil {
		return
	}
	defer ws.Close()
	// Only small events arrive here; a stricter bound than the viewer stream's
	// 64 MiB screencast budget.
	ws.SetReadLimit(chooserMaxMessageBytes)
	go func() { <-ctx.Done(); _ = ws.Close() }()
	for i, m := range []string{"Page.enable", "Page.setInterceptFileChooserDialog"} {
		params := map[string]any{}
		if i == 1 {
			params["enabled"] = true
		}
		_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := ws.WriteJSON(map[string]any{"id": i + 1, "method": m, "params": params}); err != nil {
			return
		}
	}
	// A chooser opened before this connection is not known: start clean.
	s.clearChooser()
	// getFrameTree requests in flight, by request id, each for the chooser that
	// asked.
	trees := map[int]*FileChooserPending{}
	nextTree := chooserTreeIDBase
	for {
		_, raw, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var msg struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
			Result map[string]any `json:"result"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		if msg.Method == "" {
			// A reply to one of our frame-tree reads. A failed or missing one
			// leaves FrameURL empty, which the human attach refuses.
			if p, ok := trees[msg.ID]; ok {
				delete(trees, msg.ID)
				if u, found := frameURLFromTree(msg.Result, p.FrameID); found && u != "" {
					s.setChooserFrameURL(p, u)
				}
			}
			continue
		}
		if p := s.handleChooserEvent(msg.Method, msg.Params); p != nil && p.FrameID != "" && len(trees) < 8 {
			nextTree++
			trees[nextTree] = p
			_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := ws.WriteJSON(map[string]any{"id": nextTree, "method": "Page.getFrameTree", "params": map[string]any{}}); err != nil {
				return
			}
		}
	}
}
