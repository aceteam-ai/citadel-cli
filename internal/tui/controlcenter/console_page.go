package controlcenter

import (
	"fmt"
	"runtime"
	"strings"
	"sync"

	"github.com/aceteam-ai/citadel-cli/internal/console"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// ConsolePage provides an embedded terminal (PTY) in the TUI with a
// built-in session multiplexer. Multiple shell sessions can be created
// and switched between using a Ctrl+B prefix key (like tmux).
//
// Key bindings (after Ctrl+B prefix):
//
//	c       - Create new session
//	n       - Next session
//	p       - Previous session
//	1-9     - Jump to session N
//	d       - Close current session
//	Ctrl+B  - Send literal Ctrl+B to shell
//	?       - Show help
//
// Limitations (v1):
//   - tview.TextView is line-oriented, not a terminal emulator.
//     Full-screen programs (vim, htop, etc.) will render incorrectly.
//   - Windows is not supported; the page is hidden on that platform.
type ConsolePage struct {
	app      *tview.Application
	rootFlex *tview.Flex
	tabView  *tview.TextView
	sessions *sessionManager
	streamer *console.Streamer

	mu          sync.Mutex
	active      bool
	closed      bool
	prefixOn    bool // true when Ctrl+B was just pressed, awaiting command key
	currentView tview.Primitive
}

// NewConsolePage creates a ConsolePage with an optional Streamer for
// broadcasting PTY output to WebSocket viewers. Pass nil if streaming
// is not needed.
func NewConsolePage(streamer *console.Streamer) *ConsolePage {
	if streamer == nil {
		streamer = console.NewStreamer()
	}
	return &ConsolePage{
		streamer: streamer,
	}
}

func (c *ConsolePage) Name() string  { return "console" }
func (c *ConsolePage) Title() string { return "Console" }

func (c *ConsolePage) Build(app *tview.Application) tview.Primitive {
	c.app = app
	c.sessions = newSessionManager(app)

	c.tabView = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft)

	c.rootFlex = tview.NewFlex().SetDirection(tview.FlexRow)

	if runtime.GOOS == "windows" {
		view := tview.NewTextView().SetDynamicColors(true)
		view.SetBorder(true).SetTitle(" Console ").SetTitleAlign(tview.AlignLeft)
		view.SetText("[red]Embedded console is not supported on Windows.[-]")
		c.rootFlex.AddItem(view, 0, 1, true)
	} else {
		// Create the first session automatically
		idx := c.sessions.createSession(c.streamer)
		s := c.sessions.getSession(idx)
		s.view.SetBorder(true).
			SetTitle(" Console ").
			SetTitleAlign(tview.AlignLeft)

		c.currentView = s.view
		c.rootFlex.AddItem(c.tabView, 1, 0, false)
		c.rootFlex.AddItem(s.view, 0, 1, true)
	}

	c.updateTabBar()
	return c.rootFlex
}

// OnActivate is called when the Console tab gains focus.
// Auto-starts the first shell on first activation.
func (c *ConsolePage) OnActivate() {
	c.mu.Lock()
	c.active = true
	c.mu.Unlock()

	// Auto-start the first session if not yet started
	s := c.sessions.activeSession()
	if s != nil && !s.started && !s.closed {
		_ = c.sessions.startSession(c.sessions.activeIndex())
	}
}

// OnDeactivate is called when the Console tab loses focus.
func (c *ConsolePage) OnDeactivate() {
	c.mu.Lock()
	c.active = false
	c.prefixOn = false
	c.mu.Unlock()
}

// HandleInput captures keystrokes and routes them through the
// prefix-key state machine or directly to the active PTY.
func (c *ConsolePage) HandleInput(event *tcell.EventKey) *tcell.EventKey {
	if runtime.GOOS == "windows" {
		return event
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return event
	}

	// Prefix key state machine
	if c.prefixOn {
		c.prefixOn = false
		c.mu.Unlock()
		return c.handlePrefixCommand(event)
	}

	// Detect Ctrl+B (prefix key)
	if event.Key() == tcell.KeyCtrlB {
		c.prefixOn = true
		c.mu.Unlock()
		c.showPrefixIndicator()
		return nil
	}
	c.mu.Unlock()

	s := c.sessions.activeSession()
	if s == nil {
		return event
	}

	// Start session on first keypress if needed (restart after ended)
	if !s.started {
		idx := c.sessions.activeIndex()
		_ = c.sessions.startSession(idx)
		return nil
	}

	if s.pty == nil || s.pty.IsClosed() {
		return event
	}

	data := keyToBytes(event)
	if data != nil {
		_, _ = s.pty.Write(data)
		return nil
	}

	return event
}

// handlePrefixCommand processes the key after Ctrl+B.
func (c *ConsolePage) handlePrefixCommand(event *tcell.EventKey) *tcell.EventKey {
	// Ctrl+B again: send literal Ctrl+B to shell
	if event.Key() == tcell.KeyCtrlB {
		s := c.sessions.activeSession()
		if s != nil && s.pty != nil && !s.pty.IsClosed() {
			s.pty.Write([]byte{0x02})
		}
		c.clearPrefixIndicator()
		return nil
	}

	if event.Key() == tcell.KeyRune {
		switch event.Rune() {
		case 'c': // Create new session
			c.createNewSession()
			return nil
		case 'n': // Next session
			c.sessions.next()
			c.switchView()
			return nil
		case 'p': // Previous session
			c.sessions.prev()
			c.switchView()
			return nil
		case 'd': // Close current session
			c.closeCurrentSession()
			return nil
		case '?': // Help
			c.showHelp()
			return nil
		}

		// 1-9: jump to session
		digit := int(event.Rune() - '0')
		if digit >= 1 && digit <= 9 && digit <= c.sessions.count() {
			c.sessions.switchTo(digit - 1)
			c.switchView()
			return nil
		}
	}

	c.clearPrefixIndicator()
	return nil
}

// createNewSession adds and starts a new shell session.
func (c *ConsolePage) createNewSession() {
	idx := c.sessions.createSession(c.streamer)
	s := c.sessions.getSession(idx)
	s.view.SetBorder(true).
		SetTitle(" Console ").
		SetTitleAlign(tview.AlignLeft)

	c.sessions.switchTo(idx)
	_ = c.sessions.startSession(idx)
	c.switchView()
}

// closeCurrentSession closes the active session.
func (c *ConsolePage) closeCurrentSession() {
	idx := c.sessions.activeIndex()
	if idx < 0 {
		return
	}

	c.sessions.closeSession(idx)

	if c.sessions.count() == 0 {
		// Create a new session to replace
		c.createNewSession()
		return
	}

	c.switchView()
}

// switchView replaces the displayed view with the active session's view.
func (c *ConsolePage) switchView() {
	s := c.sessions.activeSession()
	if s == nil {
		return
	}

	c.app.QueueUpdateDraw(func() {
		if c.currentView != nil {
			c.rootFlex.RemoveItem(c.currentView)
		}
		c.currentView = s.view
		c.rootFlex.AddItem(s.view, 0, 1, true)
		c.updateTabBar()
	})
}

func (c *ConsolePage) updateTabBar() {
	bar := c.sessions.tabBar()
	if bar == "" {
		c.tabView.SetText("")
	} else {
		c.tabView.SetText(bar)
	}
}

func (c *ConsolePage) showPrefixIndicator() {
	c.app.QueueUpdateDraw(func() {
		s := c.sessions.activeSession()
		if s != nil {
			s.view.SetTitle(" Console [yellow](Ctrl+B)[-] ")
		}
	})
}

func (c *ConsolePage) clearPrefixIndicator() {
	c.app.QueueUpdateDraw(func() {
		s := c.sessions.activeSession()
		if s != nil {
			s.view.SetTitle(" Console ")
		}
		c.updateTabBar()
	})
}

func (c *ConsolePage) showHelp() {
	c.app.QueueUpdateDraw(func() {
		s := c.sessions.activeSession()
		if s == nil {
			return
		}
		help := strings.Join([]string{
			"",
			"[yellow::b]Session multiplexer (Ctrl+B prefix)[-:-:-]",
			"",
			"  [white]Ctrl+B, c[-]  Create new session",
			"  [white]Ctrl+B, n[-]  Next session",
			"  [white]Ctrl+B, p[-]  Previous session",
			"  [white]Ctrl+B, 1-9[-] Jump to session N",
			"  [white]Ctrl+B, d[-]  Close current session",
			"  [white]Ctrl+B, Ctrl+B[-] Send literal Ctrl+B",
			"  [white]Ctrl+B, ?[-]  This help",
			"",
			"[gray]Press any key to dismiss[-]",
		}, "\n")
		fmt.Fprintln(tview.ANSIWriter(s.view), help)
		s.view.SetTitle(" Console ")
	})
}

// Streamer returns the Streamer for external access (e.g. wiring into an HTTP handler).
func (c *ConsolePage) Streamer() *console.Streamer {
	return c.streamer
}

// Close shuts down all PTY sessions and disconnects all stream clients.
func (c *ConsolePage) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()

	if c.sessions != nil {
		c.sessions.closeAll()
	}
	c.streamer.CloseAll()
}

// keyToBytes converts a tcell key event to bytes suitable for a PTY.
func keyToBytes(ev *tcell.EventKey) []byte {
	// Regular character
	if ev.Key() == tcell.KeyRune {
		r := ev.Rune()
		if ev.Modifiers()&tcell.ModAlt != 0 {
			return []byte{0x1b, byte(r)}
		}
		buf := make([]byte, 4)
		n := encodeRune(buf, r)
		return buf[:n]
	}

	// Ctrl+key combinations
	if ev.Modifiers()&tcell.ModCtrl != 0 {
		switch ev.Key() {
		case tcell.KeyCtrlA:
			return []byte{0x01}
		case tcell.KeyCtrlB:
			return []byte{0x02}
		case tcell.KeyCtrlC:
			return []byte{0x03}
		case tcell.KeyCtrlD:
			return []byte{0x04}
		case tcell.KeyCtrlE:
			return []byte{0x05}
		case tcell.KeyCtrlF:
			return []byte{0x06}
		case tcell.KeyCtrlG:
			return []byte{0x07}
		case tcell.KeyCtrlK:
			return []byte{0x0b}
		case tcell.KeyCtrlL:
			return []byte{0x0c}
		case tcell.KeyCtrlN:
			return []byte{0x0e}
		case tcell.KeyCtrlO:
			return []byte{0x0f}
		case tcell.KeyCtrlP:
			return []byte{0x10}
		case tcell.KeyCtrlQ:
			return []byte{0x11}
		case tcell.KeyCtrlR:
			return []byte{0x12}
		case tcell.KeyCtrlS:
			return []byte{0x13}
		case tcell.KeyCtrlT:
			return []byte{0x14}
		case tcell.KeyCtrlU:
			return []byte{0x15}
		case tcell.KeyCtrlV:
			return []byte{0x16}
		case tcell.KeyCtrlW:
			return []byte{0x17}
		case tcell.KeyCtrlX:
			return []byte{0x18}
		case tcell.KeyCtrlY:
			return []byte{0x19}
		case tcell.KeyCtrlZ:
			return []byte{0x1a}
		}
	}

	// Special keys -> ANSI escape sequences
	switch ev.Key() {
	case tcell.KeyEnter:
		return []byte{'\r'}
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		return []byte{0x7f}
	case tcell.KeyTab:
		return []byte{'\t'}
	case tcell.KeyEscape:
		return []byte{0x1b}
	case tcell.KeyUp:
		return []byte{0x1b, '[', 'A'}
	case tcell.KeyDown:
		return []byte{0x1b, '[', 'B'}
	case tcell.KeyRight:
		return []byte{0x1b, '[', 'C'}
	case tcell.KeyLeft:
		return []byte{0x1b, '[', 'D'}
	case tcell.KeyHome:
		return []byte{0x1b, '[', 'H'}
	case tcell.KeyEnd:
		return []byte{0x1b, '[', 'F'}
	case tcell.KeyInsert:
		return []byte{0x1b, '[', '2', '~'}
	case tcell.KeyDelete:
		return []byte{0x1b, '[', '3', '~'}
	case tcell.KeyPgUp:
		return []byte{0x1b, '[', '5', '~'}
	case tcell.KeyPgDn:
		return []byte{0x1b, '[', '6', '~'}
	}

	return nil
}

// encodeRune encodes a rune as UTF-8 into buf and returns the number of bytes written.
func encodeRune(buf []byte, r rune) int {
	if r < 0x80 {
		buf[0] = byte(r)
		return 1
	}
	if r < 0x800 {
		buf[0] = byte(0xC0 | (r >> 6))
		buf[1] = byte(0x80 | (r & 0x3F))
		return 2
	}
	if r < 0x10000 {
		buf[0] = byte(0xE0 | (r >> 12))
		buf[1] = byte(0x80 | ((r >> 6) & 0x3F))
		buf[2] = byte(0x80 | (r & 0x3F))
		return 3
	}
	buf[0] = byte(0xF0 | (r >> 18))
	buf[1] = byte(0x80 | ((r >> 12) & 0x3F))
	buf[2] = byte(0x80 | ((r >> 6) & 0x3F))
	buf[3] = byte(0x80 | (r & 0x3F))
	return 4
}
