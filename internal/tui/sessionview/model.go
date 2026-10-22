// Package sessionview is a read-only client display, never a node owner.
package sessionview

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Snapshot contains only fields published by the existing session status route.
// PID and uptime are observations, not proof of an authenticated incarnation.
type Snapshot struct {
	Mode      string
	Connected bool
	NodeName  string
	MeshIP    string
	Version   string
	PID       int64
	UptimeSec int64
}

type Fetch func(context.Context) (Snapshot, error)

type refreshMsg struct{}
type statusMsg struct {
	generation uint64
	snapshot   Snapshot
	err        error
}

// Model owns only its request context and terminal display. It exposes no stop,
// service, authentication, mesh, or configuration callbacks.
type Model struct {
	ctx         context.Context
	cancel      context.CancelFunc
	fetch       Fetch
	generation  uint64
	pending     bool
	stopped     bool
	unavailable bool
	haveStatus  bool
	status      Snapshot
}

func New(ctx context.Context, fetch Fetch) *Model {
	ctx, cancel := context.WithCancel(ctx)
	return &Model{ctx: ctx, cancel: cancel, fetch: fetch, generation: 1}
}

// NewObserved retains the caller's validated preflight observation so the
// first viewer poll cannot silently adopt a different observed owner. It adds
// no server identity or PID-reuse guarantee to the existing wire fields.
func NewObserved(ctx context.Context, fetch Fetch, status Snapshot) *Model {
	model := New(ctx, fetch)
	model.status, model.haveStatus = status, true
	return model
}

// Close cancels viewer-owned work only. The caller must also call it if the
// terminal program exits through a signal or initialization failure.
func (m *Model) Close() {
	if !m.stopped {
		m.stopped = true
		m.generation++
		m.cancel()
	}
}

func (m *Model) Init() tea.Cmd { return m.poll() }

func (m *Model) poll() tea.Cmd {
	if m.pending || m.stopped || m.unavailable {
		return nil
	}
	m.pending = true
	generation, ctx, fetch := m.generation, m.ctx, m.fetch
	return func() tea.Msg {
		status, err := fetch(ctx)
		return statusMsg{generation: generation, snapshot: status, err: err}
	}
}

func (m *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if m.stopped {
		return m, nil
	}
	switch message := message.(type) {
	case tea.KeyMsg:
		switch message.String() {
		case "q", "ctrl+c", "esc":
			m.Close()
			return m, tea.Quit
		}
	case refreshMsg:
		return m, m.poll()
	case statusMsg:
		if message.generation != m.generation || !m.pending {
			return m, nil
		}
		m.pending = false
		status := message.snapshot
		changed := m.haveStatus && (status.PID != m.status.PID || status.Mode != m.status.Mode || status.UptimeSec < m.status.UptimeSec)
		if message.err != nil || changed {
			// Never show the previous owner's identity or connected bit after a
			// failed observation. Reattachment is explicit, not silent adoption.
			m.status, m.haveStatus, m.unavailable = Snapshot{}, false, true
			m.cancel()
			return m, nil
		}
		m.status, m.haveStatus = status, true
		return m, tea.Tick(time.Second, func(time.Time) tea.Msg { return refreshMsg{} })
	}
	return m, nil
}

func displayText(value string) string {
	// Quote control, escape, bidi and non-ASCII bytes as literal ASCII. Peer
	// strings never become terminal instructions or styling markup.
	return strconv.QuoteToASCII(value)
}

func (m *Model) View() string {
	var out strings.Builder
	out.WriteString("Citadel session — read-only viewer\n\n")
	if m.unavailable {
		out.WriteString("Session unavailable or changed. Detach and run citadel session attach again.\n")
	} else if !m.haveStatus {
		out.WriteString("Reading the existing local session…\n")
	} else {
		state := "offline"
		if m.status.Connected {
			state = "connected"
		}
		fmt.Fprintf(&out, "Mode: %s\nMesh: %s\nNode: %s\nMesh IP: %s\nVersion: %s\nPID: %d\nUptime: %ds\n",
			m.status.Mode, state, displayText(m.status.NodeName), displayText(m.status.MeshIP),
			displayText(m.status.Version), m.status.PID, m.status.UptimeSec)
	}
	out.WriteString("\nq / Esc / Ctrl-C: detach this viewer; the node session is not stopped.\n")
	return out.String()
}
