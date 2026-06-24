package cmd

import (
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/desktop"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/terminal"
)

// fakeListener is a minimal net.Listener stand-in. Its Accept() returns
// whatever error the test has armed via setAcceptErr, letting the tests
// simulate a tsnet VPN listener being torn down on reconnect (which surfaces as
// an Accept error in the server's accept loop) without a real tailnet. It also
// records whether it was closed so tests can assert no listener is leaked.
type fakeListener struct {
	addr string

	mu        sync.Mutex
	closed    bool
	acceptErr error
}

func (f *fakeListener) Accept() (net.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.acceptErr != nil {
		return nil, f.acceptErr
	}
	return nil, fmt.Errorf("no connections in test")
}
func (f *fakeListener) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}
func (f *fakeListener) Addr() net.Addr { return fakeAddr(f.addr) }
func (f *fakeListener) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}
func (f *fakeListener) setAcceptErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acceptErr = err
}

type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

// resetVPNState clears all package-level VPN-listener tracking and restores the
// real network indirection hooks. Tests must call this so they don't leak state
// into other tests in the package.
func resetVPNState(t *testing.T) {
	t.Helper()
	origListen := listenVPNFn
	origConnected := isConnectedFn
	origIP := currentVPNIPFn

	clear := func() {
		ccVPNMu.Lock()
		ccVNCVPNListener = nil
		ccVNCVPNIP = ""
		ccTerminalVPNListener = nil
		ccTerminalVPNIP = ""
		ccVPNMu.Unlock()

		ccVNCRunning.Store(false)
		ccVNCServer.Store(nil)
		ccTerminalRunning.Store(false)
		ccTerminalServer.Store(nil)
		platform.ClearEmbeddedVNCPort()
	}

	t.Cleanup(func() {
		listenVPNFn = origListen
		isConnectedFn = origConnected
		currentVPNIPFn = origIP
		clear()
	})

	clear()
}

// TestAttachVNCVPNListenerIdempotent verifies that calling the attach helper
// twice while connected with a live listener binds exactly one listener and
// does not leak.
func TestAttachVNCVPNListenerIdempotent(t *testing.T) {
	resetVPNState(t)

	var dialed int
	currentIP := "100.64.0.30"
	isConnectedFn = func() bool { return true }
	currentVPNIPFn = func() (string, error) { return currentIP, nil }
	listenVPNFn = func(network, port string) (net.Listener, string, error) {
		dialed++
		return &fakeListener{addr: currentIP + ":" + port}, currentIP, nil
	}

	ccVNCServer.Store(desktop.NewVNCServer(desktop.VNCServerConfig{Host: "127.0.0.1", Port: ccVNCPort}))
	ccVNCRunning.Store(true)

	attachVNCVPNListener()
	attachVNCVPNListener() // idempotent: listener still live → no rebind

	if dialed != 1 {
		t.Fatalf("ListenVPN called %d times, want 1 (idempotent re-attach)", dialed)
	}
	if ccVNCVPNListener == nil {
		t.Fatal("ccVNCVPNListener = nil, want attached after attach")
	}
	if platform.EmbeddedVNCPort() != ccVNCPort {
		t.Errorf("EmbeddedVNCPort() = %d, want %d (heartbeat must reflect reachability)", platform.EmbeddedVNCPort(), ccVNCPort)
	}
}

// TestVPNListenerReattachAfterSameIPReconnect simulates the exact issue #317
// failure: a tsnet drop+recover where the node returns to the SAME tailnet IP
// (machine key preserved) but the VPN listener was torn down. Detection must
// not rely on an IP change — it keys on the listener's accept loop seeing the
// teardown — and the supervisor must tear down the dead listener and rebind a
// fresh one without restarting the server.
func TestVPNListenerReattachAfterSameIPReconnect(t *testing.T) {
	resetVPNState(t)

	const sameIP = "100.64.0.30" // never changes — the reported scenario
	connected := true

	isConnectedFn = func() bool { return connected }
	currentVPNIPFn = func() (string, error) {
		if !connected {
			return "", fmt.Errorf("no IPv4 address assigned")
		}
		return sameIP, nil
	}

	var created []*fakeListener
	listenVPNFn = func(network, port string) (net.Listener, string, error) {
		if !connected {
			return nil, "", fmt.Errorf("not connected to AceTeam Network")
		}
		ln := &fakeListener{addr: sameIP + ":" + port}
		created = append(created, ln)
		return ln, sameIP, nil
	}

	// Both servers running (desktop permission assumed enabled by caller).
	ccVNCServer.Store(desktop.NewVNCServer(desktop.VNCServerConfig{Host: "127.0.0.1", Port: ccVNCPort}))
	ccVNCRunning.Store(true)
	ccTerminalServer.Store(terminal.NewServer(terminal.DefaultConfig(), nil))
	ccTerminalRunning.Store(true)

	// 1. Initial attach while connected.
	attachVNCVPNListener()
	attachTerminalVPNListener()

	if len(created) != 2 {
		t.Fatalf("after initial attach, ListenVPN created %d listeners, want 2 (vnc+terminal)", len(created))
	}
	if !vpnListenersHealthy() {
		t.Fatal("vpnListenersHealthy() = false right after a successful attach")
	}
	firstVNC := created[0]
	firstTerminal := created[1]

	// 2. tsnet drops and recovers at the SAME IP, tearing down the listeners.
	//    The server accept loops observe this as an Accept error. Simulate the
	//    accept loop's next Accept() call by arming the underlying fake with a
	//    teardown error and invoking the wrapper's Accept (as the loop would).
	firstVNC.setAcceptErr(net.ErrClosed)
	firstTerminal.setAcceptErr(net.ErrClosed)
	ccVPNMu.Lock()
	_, _ = ccVNCVPNListener.Accept()      // accept loop sees teardown → marks dead
	_, _ = ccTerminalVPNListener.Accept() // ditto
	ccVPNMu.Unlock()

	// Health check must now report unhealthy even though the IP is unchanged.
	if vpnListenersHealthy() {
		t.Fatal("vpnListenersHealthy() = true after same-IP teardown; dead listener not detected")
	}

	// 3. Self-heal: re-attach idempotently. The dead listeners must be closed
	//    and replaced — at the SAME IP — without restarting the server.
	attachVNCVPNListener()
	attachTerminalVPNListener()

	if !firstVNC.isClosed() {
		t.Error("stale VNC VPN listener was not closed on re-attach (listener leak)")
	}
	if !firstTerminal.isClosed() {
		t.Error("stale terminal VPN listener was not closed on re-attach (listener leak)")
	}
	if len(created) != 4 {
		t.Fatalf("after re-attach, ListenVPN created %d listeners total, want 4", len(created))
	}
	if ccVNCVPNIP != sameIP {
		t.Errorf("ccVNCVPNIP = %q, want %q", ccVNCVPNIP, sameIP)
	}
	if !vpnListenersHealthy() {
		t.Fatal("vpnListenersHealthy() = false after self-heal re-attach")
	}
	if platform.EmbeddedVNCPort() != ccVNCPort {
		t.Errorf("EmbeddedVNCPort() = %d, want %d after re-attach (vnc_port must recover)", platform.EmbeddedVNCPort(), ccVNCPort)
	}
}

// TestVPNSupervisorReattachOnHealthLoss drives the supervisor's decision logic
// (without the ticker) to confirm a steady-state dead listener at the same IP
// triggers a re-attach — the "display==available && vnc_port==0" self-heal.
func TestVPNSupervisorReattachOnHealthLoss(t *testing.T) {
	resetVPNState(t)

	const sameIP = "100.64.0.30"
	isConnectedFn = func() bool { return true }
	currentVPNIPFn = func() (string, error) { return sameIP, nil }

	var created []*fakeListener
	listenVPNFn = func(network, port string) (net.Listener, string, error) {
		ln := &fakeListener{addr: sameIP + ":" + port}
		created = append(created, ln)
		return ln, sameIP, nil
	}

	ccVNCServer.Store(desktop.NewVNCServer(desktop.VNCServerConfig{Host: "127.0.0.1", Port: ccVNCPort}))
	ccVNCRunning.Store(true)

	attachVNCVPNListener()
	if platform.EmbeddedVNCPort() != ccVNCPort {
		t.Fatalf("EmbeddedVNCPort() = %d after initial attach, want %d", platform.EmbeddedVNCPort(), ccVNCPort)
	}

	// Listener torn down at the same IP → vnc_port effectively 0.
	created[0].setAcceptErr(net.ErrClosed)
	ccVPNMu.Lock()
	_, _ = ccVNCVPNListener.Accept()
	ccVPNMu.Unlock()

	// The supervisor's steady-state branch: connected, not a fresh transition,
	// but listeners unhealthy → re-attach.
	if vpnListenersHealthy() {
		t.Fatal("expected unhealthy listeners after teardown")
	}
	attachVNCVPNListener() // what the supervisor calls on health loss

	if len(created) != 2 {
		t.Fatalf("expected a fresh listener on health-loss re-attach, created=%d", len(created))
	}
	if !vpnListenersHealthy() {
		t.Fatal("vpnListenersHealthy() = false after supervisor re-attach")
	}
}

// TestVNCPortDropsWhileListenerDeadAndDisconnected asserts the heartbeat's
// vnc_port reports 0 (unreachable) when the VPN listener is dead and the node
// cannot rebind because it is disconnected — i.e. vnc_port reflects actual VPN
// reachability, not just localhost server state (issue #317 requirement #3).
func TestVNCPortDropsWhileListenerDeadAndDisconnected(t *testing.T) {
	resetVPNState(t)

	const sameIP = "100.64.0.30"
	connected := true
	isConnectedFn = func() bool { return connected }
	currentVPNIPFn = func() (string, error) {
		if !connected {
			return "", fmt.Errorf("disconnected")
		}
		return sameIP, nil
	}
	var created []*fakeListener
	listenVPNFn = func(network, port string) (net.Listener, string, error) {
		if !connected {
			return nil, "", fmt.Errorf("not connected")
		}
		ln := &fakeListener{addr: sameIP + ":" + port}
		created = append(created, ln)
		return ln, sameIP, nil
	}

	ccVNCServer.Store(desktop.NewVNCServer(desktop.VNCServerConfig{Host: "127.0.0.1", Port: ccVNCPort}))
	ccVNCRunning.Store(true)

	attachVNCVPNListener()
	if platform.EmbeddedVNCPort() != ccVNCPort {
		t.Fatalf("EmbeddedVNCPort() = %d, want %d", platform.EmbeddedVNCPort(), ccVNCPort)
	}

	// Listener dies, and the node is now disconnected so it cannot rebind.
	created[0].setAcceptErr(net.ErrClosed)
	ccVPNMu.Lock()
	_, _ = ccVNCVPNListener.Accept()
	ccVPNMu.Unlock()
	connected = false

	attachVNCVPNListener() // supervisor attempt; can't rebind while disconnected

	if platform.EmbeddedVNCPort() != 0 {
		t.Errorf("EmbeddedVNCPort() = %d while listener dead and disconnected, want 0", platform.EmbeddedVNCPort())
	}
}

// TestAttachVNCVPNListenerSkippedWhenDisconnected verifies the attach helper is
// a no-op when not connected, leaving vnc_port at 0 (the unreachable state the
// heartbeat should report).
func TestAttachVNCVPNListenerSkippedWhenDisconnected(t *testing.T) {
	resetVPNState(t)

	isConnectedFn = func() bool { return false }
	currentVPNIPFn = func() (string, error) { return "", fmt.Errorf("not connected") }
	listenVPNFn = func(network, port string) (net.Listener, string, error) {
		t.Fatal("ListenVPN should not be called while disconnected")
		return nil, "", nil
	}

	ccVNCServer.Store(desktop.NewVNCServer(desktop.VNCServerConfig{Host: "127.0.0.1", Port: ccVNCPort}))
	ccVNCRunning.Store(true)

	attachVNCVPNListener()

	if ccVNCVPNListener != nil {
		t.Error("ccVNCVPNListener != nil while disconnected, want nil")
	}
	if platform.EmbeddedVNCPort() != 0 {
		t.Errorf("EmbeddedVNCPort() = %d while disconnected, want 0", platform.EmbeddedVNCPort())
	}
}

// TestVPNListenerSupervisorRaceWithStartStop exercises the issue #319 data race:
// the VPN-listener supervisor (vpnListenersHealthy + the attach* helpers it
// calls) reads ccVNCRunning/ccTerminalRunning/ccVNCServer/ccTerminalServer
// concurrently with the start/stop paths that write them. Before these globals
// were made atomic, those reads/writes were unsynchronized — a genuine data
// race under the Go memory model — and `go test -race` flags it. Run with
// -race; with the fix it is clean.
//
// The writer goroutines mirror startVNCServer/stopVNCServer and
// startTerminalServer/stopTerminalServer: they publish a real (non-nil) server
// pointer and flip the running flag on, then flip it off (production stop*
// never nils the server pointer, so neither do we — that would manufacture a
// nil-deref that cannot happen in prod). The reader goroutine drives the exact
// supervisor decision path. The network indirection hooks are stubbed so attach
// never touches a real tailnet.
func TestVPNListenerSupervisorRaceWithStartStop(t *testing.T) {
	resetVPNState(t)

	const ip = "100.64.0.30"
	isConnectedFn = func() bool { return true }
	currentVPNIPFn = func() (string, error) { return ip, nil }
	listenVPNFn = func(network, port string) (net.Listener, string, error) {
		return &fakeListener{addr: ip + ":" + port}, ip, nil
	}

	// Pre-create real, non-nil servers the writers publish (mirrors what
	// startVNCServer/startTerminalServer Store after a successful Start).
	vncSrv := desktop.NewVNCServer(desktop.VNCServerConfig{Host: "127.0.0.1", Port: ccVNCPort})
	termSrv := terminal.NewServer(terminal.DefaultConfig(), nil)

	const iterations = 2000
	var wg sync.WaitGroup

	// Writer 1: start/stop the VNC server globals.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			ccVNCServer.Store(vncSrv)
			ccVNCRunning.Store(true)
			ccVNCRunning.Store(false)
		}
	}()

	// Writer 2: start/stop the terminal server globals.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			ccTerminalServer.Store(termSrv)
			ccTerminalRunning.Store(true)
			ccTerminalRunning.Store(false)
		}
	}()

	// Reader: the supervisor's steady-state decision path — health check plus
	// both idempotent re-attaches.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = vpnListenersHealthy()
			if ccTerminalRunning.Load() {
				attachTerminalVPNListener()
			}
			if ccVNCRunning.Load() {
				attachVNCVPNListener()
			}
		}
	}()

	wg.Wait()
}
