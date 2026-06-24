package controlcenter

import (
	"fmt"
	"io"
	"sync"

	"github.com/aceteam-ai/citadel-cli/internal/console"
	"github.com/rivo/tview"
)

// consoleSession wraps a single PTY session and its output state.
type consoleSession struct {
	id       int
	label    string
	pty      *console.PTYSession
	streamer *console.Streamer
	view     *tview.TextView
	stopRead chan struct{}
	started  bool
	closed   bool
}

// sessionManager tracks multiple PTY sessions for the Console tab.
type sessionManager struct {
	mu       sync.Mutex
	sessions []*consoleSession
	active   int
	nextID   int
	app      *tview.Application
}

func newSessionManager(app *tview.Application) *sessionManager {
	return &sessionManager{
		app:    app,
		active: -1,
	}
}

// createSession adds a new session and returns its index.
func (sm *sessionManager) createSession(streamer *console.Streamer) int {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.nextID++
	id := sm.nextID

	view := tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWordWrap(false).
		SetChangedFunc(func() {
			sm.app.Draw()
		})

	s := &consoleSession{
		id:       id,
		label:    "bash",
		streamer: streamer,
		view:     view,
		stopRead: make(chan struct{}),
	}
	sm.sessions = append(sm.sessions, s)
	idx := len(sm.sessions) - 1

	if sm.active < 0 {
		sm.active = idx
	}

	return idx
}

// startSession spawns a PTY for the session at idx.
func (sm *sessionManager) startSession(idx int) error {
	sm.mu.Lock()
	if idx < 0 || idx >= len(sm.sessions) {
		sm.mu.Unlock()
		return fmt.Errorf("invalid session index: %d", idx)
	}
	s := sm.sessions[idx]
	if s.started || s.closed {
		sm.mu.Unlock()
		return nil
	}
	s.started = true
	sm.mu.Unlock()

	session, err := console.NewPTYSession(console.PTYConfig{
		InitialCols: 80,
		InitialRows: 24,
	})
	if err != nil {
		sm.mu.Lock()
		s.started = false
		sm.mu.Unlock()
		return err
	}

	sm.mu.Lock()
	s.pty = session
	sm.mu.Unlock()

	go sm.readLoop(s, session)
	return nil
}

// readLoop reads PTY output and writes to the session's view and streamer.
func (sm *sessionManager) readLoop(s *consoleSession, session *console.PTYSession) {
	buf := make([]byte, 4096)
	ansiW := tview.ANSIWriter(s.view)
	// emu is a minimal single-line terminal emulator. tview's TextView is
	// line-oriented and has no cursor, so it cannot honour the in-place line
	// repaints that interactive shells (notably fish) emit on every keystroke
	// via CR + backspace + cursor-forward (CSI nC) + erase (CSI K). Feeding
	// those straight to an append-only view accumulates each repaint, so
	// typing `ls` renders as `llss`. The emulator interprets those cursor ops
	// and produces the correctly overwritten lines; it also subsumes the
	// charset-escape stripping that the old consoleFilter did (#296). It is
	// stateful across reads so sequences split over a 4096-byte chunk boundary
	// are handled correctly. The raw bytes are still broadcast UNFILTERED to
	// WebSocket viewers, which use a full terminal emulator.
	var emu lineEmulator
	// daResponder answers terminal Device Attributes queries (e.g. fish's
	// startup probe) that our line-oriented renderer would otherwise leave
	// unanswered, causing the shell to block for ~2s. See
	// device_attr_responder.go.
	var daResponder deviceAttrResponder

	defer sm.onSessionEnd(s, session)

	for {
		select {
		case <-s.stopRead:
			return
		default:
		}

		n, err := session.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])

			// Only stream to WebSocket viewers from the active session
			sm.mu.Lock()
			isActive := sm.active >= 0 && sm.active < len(sm.sessions) && sm.sessions[sm.active] == s
			sm.mu.Unlock()
			if isActive && s.streamer != nil {
				s.streamer.Broadcast(chunk)
			}

			// Reply to any Device Attributes query in this chunk by writing
			// back to the PTY (the shell's stdin), as a real terminal would.
			if reply := daResponder.scan(chunk); len(reply) > 0 {
				_, _ = session.Write(reply)
			}

			// Feed the chunk to the single-line emulator and re-render the
			// whole (bounded) buffer. The emulator returns the full rendered
			// output, so we replace the view contents rather than append; this
			// is what lets an in-place repaint overwrite instead of accumulate.
			rendered := emu.feed(chunk)
			sm.app.QueueUpdate(func() {
				s.view.Clear()
				_, _ = ansiW.Write([]byte(rendered))
				s.view.ScrollToEnd()
			})
		}
		if err != nil {
			if err != io.EOF {
				sm.app.QueueUpdateDraw(func() {
					fmt.Fprintf(s.view, "\n[red]Read error: %s[-]", err)
				})
			}
			return
		}
	}
}

// onSessionEnd handles cleanup when a PTY session ends.
func (sm *sessionManager) onSessionEnd(s *consoleSession, session *console.PTYSession) {
	if !session.IsClosed() {
		_ = session.Close()
	}

	sm.mu.Lock()
	if s.closed {
		sm.mu.Unlock()
		return
	}
	s.started = false
	s.pty = nil
	s.stopRead = make(chan struct{})
	sm.mu.Unlock()

	sm.app.QueueUpdateDraw(func() {
		fmt.Fprintln(tview.ANSIWriter(s.view), "")
		fmt.Fprintln(s.view, "[yellow]Session ended. Press any key to restart.[-]")
	})
}

// activeSession returns the currently active session (nil if none).
func (sm *sessionManager) activeSession() *consoleSession {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.active < 0 || sm.active >= len(sm.sessions) {
		return nil
	}
	return sm.sessions[sm.active]
}

// switchTo changes the active session.
func (sm *sessionManager) switchTo(idx int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if idx < 0 || idx >= len(sm.sessions) {
		return
	}
	sm.active = idx
}

// next cycles to the next session.
func (sm *sessionManager) next() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if len(sm.sessions) == 0 {
		return
	}
	sm.active = (sm.active + 1) % len(sm.sessions)
}

// prev cycles to the previous session.
func (sm *sessionManager) prev() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if len(sm.sessions) == 0 {
		return
	}
	sm.active = (sm.active - 1 + len(sm.sessions)) % len(sm.sessions)
}

// closeSession closes and removes the session at idx.
func (sm *sessionManager) closeSession(idx int) {
	sm.mu.Lock()
	if idx < 0 || idx >= len(sm.sessions) {
		sm.mu.Unlock()
		return
	}
	s := sm.sessions[idx]
	s.closed = true

	pty := s.pty
	s.pty = nil

	select {
	case <-s.stopRead:
	default:
		close(s.stopRead)
	}

	// Remove from list
	sm.sessions = append(sm.sessions[:idx], sm.sessions[idx+1:]...)

	// Adjust active index
	if len(sm.sessions) == 0 {
		sm.active = -1
	} else if sm.active >= len(sm.sessions) {
		sm.active = len(sm.sessions) - 1
	} else if sm.active > idx {
		sm.active--
	}
	sm.mu.Unlock()

	if pty != nil && !pty.IsClosed() {
		_ = pty.Close()
	}
}

// count returns the number of sessions.
func (sm *sessionManager) count() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return len(sm.sessions)
}

// activeIndex returns the active session index.
func (sm *sessionManager) activeIndex() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.active
}

// getSession returns the session at idx under lock (nil if out of bounds).
func (sm *sessionManager) getSession(idx int) *consoleSession {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if idx < 0 || idx >= len(sm.sessions) {
		return nil
	}
	return sm.sessions[idx]
}

// closeAll shuts down all sessions.
func (sm *sessionManager) closeAll() {
	sm.mu.Lock()
	sessions := make([]*consoleSession, len(sm.sessions))
	copy(sessions, sm.sessions)
	sm.sessions = nil
	sm.active = -1
	sm.mu.Unlock()

	for _, s := range sessions {
		s.closed = true
		select {
		case <-s.stopRead:
		default:
			close(s.stopRead)
		}
		if s.pty != nil && !s.pty.IsClosed() {
			_ = s.pty.Close()
		}
	}
}

// tabBar renders the session tab indicator text.
func (sm *sessionManager) tabBar() string {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if len(sm.sessions) <= 1 {
		return ""
	}

	var result string
	for i, s := range sm.sessions {
		marker := fmt.Sprintf("%d:%s", i+1, s.label)
		if i == sm.active {
			result += fmt.Sprintf("[yellow::b][%s][-:-:-] ", marker)
		} else {
			result += fmt.Sprintf("[gray]%s[-] ", marker)
		}
	}
	result += "[gray]Ctrl+B,? for help[-]"
	return result
}
