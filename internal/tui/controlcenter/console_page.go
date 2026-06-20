package controlcenter

import (
	"fmt"
	"io"
	"runtime"
	"sync"

	"github.com/aceteam-ai/citadel-cli/internal/console"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// ConsolePage provides an embedded terminal (PTY) in the TUI.
// PTY output is rendered through tview's ANSIWriter for basic color
// support and streamed to web viewers via the Streamer.
//
// Limitations (v1):
//   - tview.TextView is line-oriented, not a terminal emulator.
//     Full-screen programs (vim, htop, etc.) will render incorrectly.
//   - Windows is not supported; the page is hidden on that platform.
type ConsolePage struct {
	app      *tview.Application
	view     *tview.TextView
	streamer *console.Streamer

	mu       sync.Mutex
	pty      *console.PTYSession
	active   bool
	started  bool
	closed   bool // page-level closed flag (set by Close, prevents restart)
	stopRead chan struct{}
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
		stopRead: make(chan struct{}),
	}
}

func (c *ConsolePage) Name() string  { return "console" }
func (c *ConsolePage) Title() string { return "Console" }

func (c *ConsolePage) Build(app *tview.Application) tview.Primitive {
	c.app = app

	c.view = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWordWrap(false).
		SetChangedFunc(func() {
			app.Draw()
		})
	c.view.SetBorder(true).
		SetTitle(" Console ").
		SetTitleAlign(tview.AlignLeft)

	if runtime.GOOS == "windows" {
		c.view.SetText("[red]Embedded console is not supported on Windows.[-]")
	} else {
		c.view.SetText("[gray]Press any key to start the console...[-]")
	}

	return c.view
}

// OnActivate is called when the Console tab gains focus.
func (c *ConsolePage) OnActivate() {
	c.mu.Lock()
	c.active = true
	c.mu.Unlock()
}

// OnDeactivate is called when the Console tab loses focus.
// The PTY read loop keeps running to avoid blocking the shell.
func (c *ConsolePage) OnDeactivate() {
	c.mu.Lock()
	c.active = false
	c.mu.Unlock()
}

// HandleInput captures keystrokes and writes them to the PTY.
func (c *ConsolePage) HandleInput(event *tcell.EventKey) *tcell.EventKey {
	if runtime.GOOS == "windows" {
		return event
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return event
	}

	// Start the PTY on first keypress (or restart after session ended)
	if !c.started {
		c.started = true
		c.mu.Unlock()
		c.startPTY()
		return nil // don't forward the activation key to the shell
	}

	pty := c.pty
	c.mu.Unlock()

	if pty == nil || pty.IsClosed() {
		return event
	}

	// Convert tcell event to bytes for the PTY
	data := keyToBytes(event)
	if data != nil {
		_, _ = pty.Write(data)
		return nil // consumed
	}

	return event
}

// Streamer returns the Streamer for external access (e.g. wiring into an HTTP handler).
func (c *ConsolePage) Streamer() *console.Streamer {
	return c.streamer
}

// Close shuts down the PTY session and disconnects all stream clients.
// Safe to call from any goroutine; does not re-enter the mutex during PTY teardown.
func (c *ConsolePage) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true

	pty := c.pty
	c.pty = nil

	// Signal the read loop to stop
	select {
	case <-c.stopRead:
		// already closed
	default:
		close(c.stopRead)
	}
	c.mu.Unlock()

	// Close the PTY outside the lock to avoid deadlock
	if pty != nil && !pty.IsClosed() {
		_ = pty.Close()
	}
	c.streamer.CloseAll()
}

// startPTY spawns the shell and starts the read loop.
func (c *ConsolePage) startPTY() {
	c.view.Clear()

	session, err := console.NewPTYSession(console.PTYConfig{
		InitialCols: 80,
		InitialRows: 24,
	})
	if err != nil {
		c.app.QueueUpdateDraw(func() {
			c.view.SetText(fmt.Sprintf("[red]Failed to start console: %s[-]", err))
		})
		c.mu.Lock()
		c.started = false
		c.mu.Unlock()
		return
	}

	c.mu.Lock()
	c.pty = session
	c.mu.Unlock()

	go c.readLoop(session)
}

// readLoop continuously reads PTY output and dispatches it to the
// tview widget and the WebSocket streamer. It runs until the PTY
// closes, stopRead is signaled, or the shell exits naturally.
//
// On natural shell exit (read returns EIO/EOF), readLoop handles
// teardown: closes the PTY, resets state, and shows the restart prompt.
// This avoids the need for an onClose callback that would re-enter the mutex.
func (c *ConsolePage) readLoop(session *console.PTYSession) {
	buf := make([]byte, 4096)
	ansiW := tview.ANSIWriter(c.view)

	defer c.onSessionEnd(session)

	for {
		select {
		case <-c.stopRead:
			return
		default:
		}

		n, err := session.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])

			// Stream to WebSocket viewers (always, even when tab is inactive)
			c.streamer.Broadcast(chunk)

			// Render in the TUI view
			c.app.QueueUpdate(func() {
				_, _ = ansiW.Write(chunk)
				c.view.ScrollToEnd()
			})
		}
		if err != nil {
			if err != io.EOF {
				c.app.QueueUpdateDraw(func() {
					fmt.Fprintf(c.view, "\n[red]Read error: %s[-]", err)
				})
			}
			return
		}
	}
}

// onSessionEnd cleans up after a PTY session ends (natural exit or forced close).
// It closes the PTY, resets state so a new session can start on keypress,
// and shows the restart prompt (unless the page itself is being torn down).
func (c *ConsolePage) onSessionEnd(session *console.PTYSession) {
	// Close the PTY if it's still open (idempotent)
	if !session.IsClosed() {
		_ = session.Close()
	}

	c.mu.Lock()
	// Only reset for restart if the page isn't being torn down
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.started = false
	c.pty = nil
	c.stopRead = make(chan struct{})
	c.mu.Unlock()

	c.app.QueueUpdateDraw(func() {
		fmt.Fprintln(tview.ANSIWriter(c.view), "")
		fmt.Fprintln(c.view, "[yellow]Session ended. Press any key to restart.[-]")
	})
}

// keyToBytes converts a tcell key event to bytes suitable for a PTY.
func keyToBytes(ev *tcell.EventKey) []byte {
	// Regular character
	if ev.Key() == tcell.KeyRune {
		r := ev.Rune()
		if ev.Modifiers()&tcell.ModAlt != 0 {
			// Alt+key: ESC prefix
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
