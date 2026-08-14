package controlcenter

import (
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// TestPeerModalKeyHandler_EnterConnects pins issue #761: Enter on the Network
// Peers modal must route to connect, not ping. peerModalKeyHandler is the
// exact function wired into the modal table's SetInputCapture in
// showPeerDetailModal, so this exercises the real routing decision without a
// live tview screen or mesh connection: the connect/ping/close callbacks are
// fakes.
func TestPeerModalKeyHandler_EnterConnects(t *testing.T) {
	var connectCalled, pingCalled, closeCalled bool
	cc := &ControlCenter{}
	handler := cc.peerModalKeyHandler(
		func() { connectCalled = true },
		func() { pingCalled = true },
		func() { closeCalled = true },
	)

	result := handler(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	if !connectCalled {
		t.Error("Enter did not call connectSelected")
	}
	if pingCalled {
		t.Error("Enter called pingSelected, it should route to connect only (issue #761)")
	}
	if closeCalled {
		t.Error("Enter called closeModal unexpectedly")
	}
	if result != nil {
		t.Error("Enter should consume the event (return nil)")
	}
}

// TestPeerModalKeyHandler_PStillPings pins that 'p' and 'P' remain bound to
// ping and do not also trigger connect, unchanged by issue #761.
func TestPeerModalKeyHandler_PStillPings(t *testing.T) {
	for _, r := range []rune{'p', 'P'} {
		r := r
		t.Run(string(r), func(t *testing.T) {
			var connectCalled, pingCalled, closeCalled bool
			cc := &ControlCenter{}
			handler := cc.peerModalKeyHandler(
				func() { connectCalled = true },
				func() { pingCalled = true },
				func() { closeCalled = true },
			)

			result := handler(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))

			if !pingCalled {
				t.Errorf("%q did not call pingSelected", r)
			}
			if connectCalled {
				t.Errorf("%q called connectSelected, only Enter should", r)
			}
			if closeCalled {
				t.Errorf("%q called closeModal unexpectedly", r)
			}
			if result != nil {
				t.Errorf("%q should consume the event (return nil)", r)
			}
		})
	}
}

// TestPeerModalKeyHandler_EscCloses pins that Esc still closes the modal and
// does not connect or ping.
func TestPeerModalKeyHandler_EscCloses(t *testing.T) {
	var connectCalled, pingCalled, closeCalled bool
	cc := &ControlCenter{}
	handler := cc.peerModalKeyHandler(
		func() { connectCalled = true },
		func() { pingCalled = true },
		func() { closeCalled = true },
	)

	result := handler(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))

	if !closeCalled {
		t.Error("Esc did not call closeModal")
	}
	if connectCalled || pingCalled {
		t.Error("Esc should not connect or ping")
	}
	if result != nil {
		t.Error("Esc should consume the event (return nil)")
	}
}

// TestPeerModalKeyHandler_OtherKeyPassesThrough pins that a key with no
// binding (e.g. an arrow key, used for table navigation) is passed through
// unconsumed rather than triggering any action.
func TestPeerModalKeyHandler_OtherKeyPassesThrough(t *testing.T) {
	var connectCalled, pingCalled, closeCalled bool
	cc := &ControlCenter{}
	handler := cc.peerModalKeyHandler(
		func() { connectCalled = true },
		func() { pingCalled = true },
		func() { closeCalled = true },
	)

	event := tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
	result := handler(event)

	if connectCalled || pingCalled || closeCalled {
		t.Error("an unbound key should not trigger connect, ping, or close")
	}
	if result != event {
		t.Error("an unbound key should be passed through unchanged so table navigation keeps working")
	}
}

// TestShowPeerDetailModal_EnterInvokesConnectToPeerFn is the higher-fidelity
// companion to the peerModalKeyHandler tests above: it builds the real modal
// via showPeerDetailModal (not a hand-rolled handler) and presses Enter
// through the table's actual installed SetInputCapture, proving the
// production wiring, not just the extracted routing function, calls
// connectToPeerFn with the selected peer's IP and never touches network
// ping. Uses a tcell simulation screen (no real terminal) and a fake
// connectToPeerFn (no live mesh), following the pattern already used by
// TestWhatsAppDoDeployRunsWorkOffGoroutine for driving a real tview event
// loop in tests.
func TestShowPeerDetailModal_EnterInvokesConnectToPeerFn(t *testing.T) {
	var mu sync.Mutex
	var gotIP string
	var connectCalls int

	app := tview.NewApplication()
	app.SetScreen(tcell.NewSimulationScreen(""))

	cc := &ControlCenter{
		app:      app,
		rootView: tview.NewBox(),
		data: StatusData{
			Connected: true,
			Peers: []PeerInfo{
				{Hostname: "node-a", IP: "100.64.0.10", Online: true},
			},
		},
		peersView: tview.NewTable(),
		connectToPeerFn: func(ip string) error {
			mu.Lock()
			gotIP = ip
			connectCalls++
			mu.Unlock()
			return nil
		},
	}
	cc.peersView.Select(1, 0)

	appDone := make(chan struct{})
	go func() { _ = app.Run(); close(appDone) }()
	defer func() { app.Stop(); <-appDone }()

	// Build the modal and press Enter inside a single QueueUpdate so both run
	// on the tview event-loop goroutine, exactly like a real keypress would
	// (see TestWhatsAppDoDeployRunsWorkOffGoroutine for why this matters:
	// touching tview widgets from a second goroutine while Run() is active
	// races with its internal draw/event handling).
	done := make(chan struct{})
	app.QueueUpdate(func() {
		defer close(done)
		cc.showPeerDetailModal()
		table, ok := cc.app.GetFocus().(*tview.Table)
		if !ok {
			return
		}
		capture := table.GetInputCapture()
		if capture == nil {
			return
		}
		capture(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("modal build + Enter press did not complete within 2s")
	}

	mu.Lock()
	ip, calls := gotIP, connectCalls
	mu.Unlock()

	if calls != 1 {
		t.Fatalf("connectToPeerFn called %d times, want 1", calls)
	}
	if ip != "100.64.0.10" {
		t.Fatalf("connectToPeerFn called with IP %q, want %q", ip, "100.64.0.10")
	}
}
