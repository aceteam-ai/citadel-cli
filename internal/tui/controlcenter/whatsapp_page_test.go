package controlcenter

import (
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// TestWhatsAppHandleInputNeverBlocks is the deadlock guard for the class of bug
// fixed in #402 (chat) and re-audited here: an action branch in HandleInput must
// NEVER touch a QueueUpdateDraw/render path synchronously on the tview event-loop
// goroutine. The old code called p.doDeploy()/p.doStop() directly, and both call
// queueRender -> app.QueueUpdateDraw, whose synchronous channel receive can only
// be drained by the very event loop the call is running on -> permanent hang that
// freezes the whole Control Center.
//
// To make the hang reproducible without a running tview event loop, we give the
// page a real *tview.Application (so queueRender is NOT a nil-app no-op) and start
// no goroutine to drain QueueUpdateDraw. On the OLD code the synchronous
// doDeploy/doStop would block forever inside QueueUpdateDraw; on the NEW code
// HandleInput dispatches `go p.doX()` and returns promptly, so the handler call
// completes well within the timeout.
func TestWhatsAppHandleInputNeverBlocks(t *testing.T) {
	// Deploy/Stop block briefly to model a real action, so if HandleInput ran them
	// synchronously the handler would not return promptly even ignoring the
	// QueueUpdateDraw hang.
	deployStarted := make(chan struct{}, 1)
	cb := WhatsAppCallbacks{
		Status: func() WhatsAppStatus { return WhatsAppStatus{} },
		Deploy: func() (string, string, error) {
			select {
			case deployStarted <- struct{}{}:
			default:
			}
			time.Sleep(200 * time.Millisecond)
			return "", "", nil
		},
		Stop:     func() error { time.Sleep(200 * time.Millisecond); return nil },
		QRBlocks: func() (string, error) { return "", nil },
	}
	p := NewWhatsAppPage(cb)
	// Build with a real application so queueRender exercises QueueUpdateDraw rather
	// than no-oping on a nil app. We never call app.Run(), so nothing drains the
	// QueueUpdateDraw queue — a synchronous QueueUpdateDraw from this goroutine
	// would block forever, exactly reproducing the old deadlock.
	p.Build(tview.NewApplication())

	actionKeys := []rune{'1', '2', '3', '4'}
	for _, key := range actionKeys {
		key := key
		done := make(chan struct{})
		go func() {
			p.HandleInput(tcell.NewEventKey(tcell.KeyRune, key, tcell.ModNone))
			close(done)
		}()
		select {
		case <-done:
			// HandleInput returned promptly — good.
		case <-time.After(2 * time.Second):
			t.Fatalf("HandleInput('%c') did not return within 2s — likely a synchronous QueueUpdateDraw deadlock (the #402 class)", key)
		}
	}
}

// TestWhatsAppDoDeployRunsWorkOffGoroutine asserts doDeploy performs its blocking
// work off the calling goroutine. Combined with HandleInput dispatching `go
// p.doDeploy()`, this proves the action key both returns immediately AND actually
// kicks off the deploy — so the guard is not passing merely because the action is
// a no-op.
func TestWhatsAppDoDeployRunsWorkOffGoroutine(t *testing.T) {
	var mu sync.Mutex
	deployCalled := false
	release := make(chan struct{})
	cb := WhatsAppCallbacks{
		Status: func() WhatsAppStatus { return WhatsAppStatus{} },
		Deploy: func() (string, string, error) {
			mu.Lock()
			deployCalled = true
			mu.Unlock()
			<-release // hold the deploy open so the test can observe it mid-flight
			return "", "", nil
		},
		QRBlocks: func() (string, error) { return "", nil },
	}
	p := NewWhatsAppPage(cb)
	app := tview.NewApplication()
	// A simulation screen lets the tview event loop run headless so QueueUpdateDraw
	// callbacks (from queueRender) drain and doDeploy can reach its inner deploy
	// goroutine. Stopped at test end.
	app.SetScreen(tcell.NewSimulationScreen(""))
	p.Build(app)
	app.SetRoot(p.root, true)

	appDone := make(chan struct{})
	go func() { _ = app.Run(); close(appDone) }()
	defer func() { app.Stop(); <-appDone }()

	done := make(chan struct{})
	go func() {
		p.HandleInput(tcell.NewEventKey(tcell.KeyRune, '1', tcell.ModNone))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("HandleInput('1') blocked — deploy must run off the event-loop goroutine")
	}

	// The deploy should be running in the background even though HandleInput has
	// already returned.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		called := deployCalled
		mu.Unlock()
		if called {
			break
		}
		select {
		case <-deadline:
			close(release)
			t.Fatal("deploy work never started off-goroutine")
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(release)
}
