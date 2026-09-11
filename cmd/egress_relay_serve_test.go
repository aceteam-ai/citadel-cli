// cmd/egress_relay_serve_test.go
//
// Hermetic tests for `citadel egress-relay serve` (citadel #1033). These never
// touch the live mesh: the two network probes are injected, and the VPN
// listener is swapped (egressRelayListenVPN) for a real loopback listener plus
// a fake mesh IP. The point they pin is that the serve path starts the SHARED
// egress-relay listener and constructs NO Redis job source -- the regression
// #1033 fixes.
package cmd

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// trackedListener wraps a real listener so a test can observe teardown (the
// relay's Serve closes the listener on ctx cancel).
type trackedListener struct {
	net.Listener
	once   sync.Once
	closed chan struct{}
}

func (t *trackedListener) Close() error {
	t.once.Do(func() { close(t.closed) })
	return t.Listener.Close()
}

// fakeListenVPN returns an egressRelayListenVPN replacement that binds a real
// loopback listener (so egressrelay.New/Serve run for real against it) and
// reports a fake mesh IP. It records how many times it was called and exposes
// a channel closed when the returned listener is closed.
func fakeListenVPN(t *testing.T) (fn func(string, string) (net.Listener, string, error), calls *int, closed chan struct{}) {
	t.Helper()
	calls = new(int)
	closed = make(chan struct{})
	fn = func(network, port string) (net.Listener, string, error) {
		*calls++
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, "", err
		}
		return &trackedListener{Listener: ln, closed: closed}, "100.64.0.9", nil
	}
	return fn, calls, closed
}

// swapRelaySeams swaps the package-level network seams for the duration of a
// test and restores them afterward.
func swapRelaySeams(t *testing.T, connected bool, listen func(string, string) (net.Listener, string, error)) {
	t.Helper()
	origConn, origListen := egressRelayIsConnected, egressRelayListenVPN
	egressRelayIsConnected = func() bool { return connected }
	egressRelayListenVPN = listen
	t.Cleanup(func() {
		egressRelayIsConnected = origConn
		egressRelayListenVPN = origListen
	})
}

func TestEgressRelayServeCommandRegistered(t *testing.T) {
	var found bool
	for _, c := range egressRelayCmd.Commands() {
		if c.Name() == "serve" {
			found = true
			if c.RunE == nil {
				t.Error("serve subcommand has no RunE")
			}
			if err := c.Args(c, []string{}); err != nil {
				t.Errorf("serve should accept no args: %v", err)
			}
			if err := c.Args(c, []string{"extra"}); err == nil {
				t.Error("serve should reject positional args (cobra.NoArgs)")
			}
			break
		}
	}
	if !found {
		t.Fatal("expected a 'serve' subcommand registered under 'egress-relay'")
	}
}

// TestEgressRelayServe_NotLoggedIn pins that a node with no network identity
// fails fast with an actionable error and never even reaches the connect probe
// or the listener -- the relay is mesh-only.
func TestEgressRelayServe_NotLoggedIn(t *testing.T) {
	fakeListen, calls, _ := fakeListenVPN(t)
	swapRelaySeams(t, true, fakeListen)

	verifyCalled := false
	verify := func(context.Context) (bool, error) {
		verifyCalled = true
		return true, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := egressRelayServe(ctx, cancel, func() bool { return false }, verify, nil)
	if err == nil {
		t.Fatal("expected an error when the node is not logged in")
	}
	if !strings.Contains(err.Error(), "citadel login") {
		t.Errorf("error should point at 'citadel login', got: %v", err)
	}
	if verifyCalled {
		t.Error("verify must not run when there is no network state")
	}
	if *calls != 0 {
		t.Errorf("listener must not be created when not logged in, got %d calls", *calls)
	}
}

// TestEgressRelayServe_StartsSharedListenerThenStopsOnSignal is the core #1033
// regression pin: the serve path starts the SHARED egress-relay listener
// exactly once (no duplicated authz/policy wiring), constructs NO Redis client
// or job source (none is among its enumerated dependencies), and shuts down
// cleanly on a signal -- closing the listener via ctx cancel.
func TestEgressRelayServe_StartsSharedListenerThenStopsOnSignal(t *testing.T) {
	// Short-circuit resolveEgressAllowLAN's config-dir read (see the resolve
	// test file's note) so this never reads a real node's egress-relay.yaml.
	t.Setenv("CITADEL_EGRESS_ALLOW_LAN", "0")

	fakeListen, calls, closed := fakeListenVPN(t)
	swapRelaySeams(t, true, fakeListen)

	hasState := func() bool { return true }
	verify := func(context.Context) (bool, error) { return true, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Preload the signal so the core consumes it right after starting the
	// listener (the core runs connect -> start listener -> <-sigs sequentially).
	sigs := make(chan os.Signal, 1)
	sigs <- syscall.SIGTERM

	err := egressRelayServe(ctx, cancel, hasState, verify, sigs)
	if err != nil {
		t.Fatalf("egressRelayServe returned error: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("expected the shared egress-relay listener to be started exactly once, got %d", *calls)
	}

	// Teardown is real: cancel() closed the relay listener via Serve's ctx.Done.
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the relay listener to be closed on shutdown")
	}
}

// TestEgressRelayServe_ConnectErrorSurfaces pins that a non-stale
// VerifyOrReconnect error is surfaced (not swallowed) and the listener is never
// started.
func TestEgressRelayServe_ConnectErrorSurfaces(t *testing.T) {
	fakeListen, calls, _ := fakeListenVPN(t)
	swapRelaySeams(t, false, fakeListen)

	hasState := func() bool { return true }
	verify := func(context.Context) (bool, error) { return false, errors.New("boom") }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := egressRelayServe(ctx, cancel, hasState, verify, nil)
	if err == nil {
		t.Fatal("expected the connect error to surface")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected the underlying connect error, got: %v", err)
	}
	if *calls != 0 {
		t.Errorf("listener must not start on a connect failure, got %d calls", *calls)
	}
}

// TestStartEgressRelayUsesSharedListener pins that `citadel work`'s
// startEgressRelay goes through the SAME startEgressRelayListener seam the
// serve path uses -- swapping the one egressRelayListenVPN seam is observed by
// both, which is the proof there is a single relay-start implementation.
func TestStartEgressRelayUsesSharedListener(t *testing.T) {
	t.Setenv("CITADEL_EGRESS_RELAY", "1")     // force-enable via env (no disk read)
	t.Setenv("CITADEL_EGRESS_ALLOW_LAN", "0") // short-circuit allow-lan config read

	fakeListen, calls, _ := fakeListenVPN(t)
	swapRelaySeams(t, true, fakeListen)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stops the background Serve goroutine

	startEgressRelay(ctx)

	if *calls != 1 {
		t.Fatalf("expected startEgressRelay to start the shared listener once, got %d", *calls)
	}
}
