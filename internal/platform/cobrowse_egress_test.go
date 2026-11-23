package platform

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/browserurl"
)

// egressEnv records the launch options every launch received and the guards
// the manager started. Mutates package vars: not parallel-safe.
type egressEnv struct {
	mu       sync.Mutex
	opts     []LaunchOptions
	guards   []*browserurl.EgressGuard
	launches int
	stops    int
	startErr error
	routeErr error
	routed   int
	gate     chan struct{}
}

func newEgressEnv(t *testing.T) *egressEnv {
	t.Helper()
	e := &egressEnv{}
	prevLaunch, prevStart, prevRoute := launchCobrowseProc, startSessionEgressGuard, egressRouteCheck
	egressRouteCheck = func(proc *cobrowseProc, g *browserurl.EgressGuard) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.routed++
		return e.routeErr
	}
	launchCobrowseProc = func(profileDir, startURL string, lo LaunchOptions) (*cobrowseProc, error) {
		if e.gate != nil {
			<-e.gate
		}
		e.mu.Lock()
		e.launches++
		n := e.launches
		e.opts = append(e.opts, lo)
		e.mu.Unlock()
		return &cobrowseProc{
			debugPort: 9100 + n, display: ":" + strconv.Itoa(70+n),
			browserPID: 100000 + n, xvfbPID: 200000 + n,
			exited: make(chan struct{}),
			stop: func() error {
				e.mu.Lock()
				e.stops++
				e.mu.Unlock()
				return nil
			},
		}, nil
	}
	startSessionEgressGuard = func(onRefused func(scheme, host string)) (*browserurl.EgressGuard, error) {
		if e.startErr != nil {
			return nil, e.startErr
		}
		g, err := browserurl.StartEgressGuardWithOptions(browserurl.EgressGuardOptions{OnRefused: onRefused})
		if err == nil {
			e.mu.Lock()
			e.guards = append(e.guards, g)
			e.mu.Unlock()
		}
		return g, err
	}
	t.Cleanup(func() {
		launchCobrowseProc, startSessionEgressGuard, egressRouteCheck = prevLaunch, prevStart, prevRoute
		e.mu.Lock()
		for _, g := range e.guards {
			g.Close()
		}
		e.mu.Unlock()
	})
	return e
}

func (e *egressEnv) last() LaunchOptions {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.opts[len(e.opts)-1]
}

// guardListening reports whether a guard's proxy port still accepts.
func guardListening(g *browserurl.EgressGuard) bool {
	c, err := net.DialTimeout("tcp", strings.TrimPrefix(g.ProxyURL(), "http://"), time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func setNodeEgress(t *testing.T, f func() bool) {
	t.Helper()
	nodeEgressMu.Lock()
	prev := nodeEgressRequired
	nodeEgressRequired = f
	nodeEgressMu.Unlock()
	t.Cleanup(func() {
		nodeEgressMu.Lock()
		nodeEgressRequired = prev
		nodeEgressMu.Unlock()
	})
}

// TestEgressActivation covers absent, false and true: only true launches the
// browser behind a loopback guard and reports egress public_only.
func TestEgressActivation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source func() bool
		want   bool
	}{
		{"absent", nil, false},
		{"false", func() bool { return false }, false},
		{"true", func() bool { return true }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEgressEnv(t)
			setNodeEgress(t, tc.source)
			m := adoptMgr(trustedBaseDir(t)) // node-wide shaped
			defer m.StopAll()
			st, err := m.StartSession("")
			if err != nil {
				t.Fatal(err)
			}
			lo := e.last()
			if got := lo.ProxyServer != ""; got != tc.want {
				t.Fatalf("proxy set = %v (%q), want %v", got, lo.ProxyServer, tc.want)
			}
			if tc.want {
				if !strings.HasPrefix(lo.ProxyServer, "http://127.0.0.1:") {
					t.Fatalf("guard is not on loopback: %q", lo.ProxyServer)
				}
				if lo.Quiet {
					t.Fatal("public-only must not change other launch options")
				}
				if st.Egress != EgressPublicOnly {
					t.Fatalf("status egress = %q", st.Egress)
				}
				if s2, _ := m.SessionStatus(st.ID); s2.Egress != EgressPublicOnly {
					t.Fatalf("later status egress = %q", s2.Egress)
				}
			} else if st.Egress != "" || len(e.guards) != 0 {
				t.Fatalf("no guard expected: egress=%q guards=%d", st.Egress, len(e.guards))
			}
		})
	}
}

// TestEgressReadLivePerSessionStart: the setting is re-read for every start.
func TestEgressReadLivePerSessionStart(t *testing.T) {
	e := newEgressEnv(t)
	on := false
	m := adoptMgr(trustedBaseDir(t))
	m.SetEgressRequired(func() bool { return on })
	defer m.StopAll()
	a, _ := m.StartSession("")
	on = true
	b, _ := m.StartSession("")
	if a.Egress != "" || b.Egress != EgressPublicOnly {
		t.Fatalf("egress a=%q b=%q", a.Egress, b.Egress)
	}
	if len(e.guards) != 1 {
		t.Fatalf("guards = %d", len(e.guards))
	}
}

// TestEgressGuardFailureFailsClosed: when the guard cannot start, the session
// is refused before any browser launches, and no slot or dir is left.
func TestEgressGuardFailureFailsClosed(t *testing.T) {
	e := newEgressEnv(t)
	e.startErr = errors.New("no loopback")
	setNodeEgress(t, func() bool { return true })
	base := trustedBaseDir(t)
	m := adoptMgr(base)
	_, err := m.StartSession("https://a.example")
	if err == nil || !strings.Contains(err.Error(), "egress guard could not start") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if e.launches != 0 {
		t.Fatalf("browser launched %d time(s) without its guard", e.launches)
	}
	if n := len(m.List()); n != 0 {
		t.Fatalf("%d session(s) left after a refused start", n)
	}
	entries, _ := os.ReadDir(base)
	if len(entries) != 0 {
		t.Fatalf("session dir left behind: %v", entries)
	}
}

// TestLocalModeIgnoresNodeEgress: an isolated (local-mode) manager never reads
// the node setting; its own launch options are passed through unchanged.
func TestLocalModeIgnoresNodeEgress(t *testing.T) {
	e := newEgressEnv(t)
	setNodeEgress(t, func() bool { return true })
	m := newCobrowseSessionManager(trustedBaseDir(t), 2)
	want := LaunchOptions{ProxyServer: "http://127.0.0.1:1", Quiet: true}
	m.SetLaunchOptions(want)
	defer m.StopAll()
	st, err := m.StartSession("")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.last(); got != want {
		t.Fatalf("local launch options changed: %+v", got)
	}
	if st.Egress != "" || len(e.guards) != 0 {
		t.Fatalf("local mode got a node guard: egress=%q guards=%d", st.Egress, len(e.guards))
	}
}

// TestEgressGuardClosedWithSession: the guard stops with its browser, on stop
// and on a stop that lands while the browser is still launching.
func TestEgressGuardClosedWithSession(t *testing.T) {
	e := newEgressEnv(t)
	setNodeEgress(t, func() bool { return true })
	m := adoptMgr(trustedBaseDir(t))
	st, err := m.StartSession("")
	if err != nil {
		t.Fatal(err)
	}
	g := e.guards[0]
	if !guardListening(g) {
		t.Fatal("guard not listening while the session runs")
	}
	if err := m.Stop(st.ID); err != nil {
		t.Fatal(err)
	}
	if guardListening(g) {
		t.Fatal("guard still listening after stop")
	}

	// Stopped during launch: the launcher is held until StopAll has run.
	e.gate = make(chan struct{})
	done := make(chan error, 1)
	go func() { _, err := m.StartSession(""); done <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.mu.Lock()
		n := len(e.guards)
		e.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second guard never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = m.StopAll()
	close(e.gate)
	if err := <-done; err == nil {
		t.Fatal("a session stopped during launch should report it")
	}
	if guardListening(e.guards[1]) {
		t.Fatal("guard of a session stopped during launch is still listening")
	}
}

// TestEgressSessionRunsOnHost: a guarded session never uses the container or
// a detached scope; the container's network cannot reach the loopback guard,
// and a detached browser would outlive it.
func TestEgressSessionRunsOnHost(t *testing.T) {
	e := newEgressEnv(t)
	prevRunner, prevDet := activeBrowserRunner, detachedWorker.Load()
	activeBrowserRunner = choosingRunner{}
	detachedWorker.Store(true)
	t.Cleanup(func() { activeBrowserRunner = prevRunner; detachedWorker.Store(prevDet) })

	m := adoptMgr(trustedBaseDir(t))
	defer m.StopAll()
	on := false
	m.SetEgressRequired(func() bool { return on })
	if _, err := m.StartSession(""); err != nil {
		t.Fatal(err)
	}
	if lo := e.last(); !lo.UseContainer || !lo.Detach {
		t.Fatalf("control: without public-only this manager uses the container and detaches: %+v", lo)
	}
	on = true
	if _, err := m.StartSession(""); err != nil {
		t.Fatal(err)
	}
	if lo := e.last(); lo.UseContainer || lo.Detach || lo.ProxyServer == "" {
		t.Fatalf("public-only session must run directly on the host behind the guard: %+v", lo)
	}
}

// choosingRunner is a host runner that would pick the container for every
// node-wide launch.
type choosingRunner struct{}

func (choosingRunner) Name() string                   { return "choosing" }
func (choosingRunner) Discover() (BrowserInfo, error) { return BrowserInfo{Path: "x"}, nil }
func (choosingRunner) PrepareBaseDir(string) error    { return nil }
func (choosingRunner) SweepOrphans(string)            {}
func (choosingRunner) Launch(string, string, LaunchOptions) (*cobrowseProc, error) {
	return nil, errors.New("not used")
}
func (choosingRunner) useContainer(nodeWide bool) bool { return nodeWide }

// TestEgressSessionIsNeverAdopted: the record is marked, a restart does not
// re-adopt it, and a self-restart stops it even where adoption is supported.
func TestEgressSessionIsNeverAdopted(t *testing.T) {
	newAdoptEnv(t)
	newEgressEnv(t)
	setNodeEgress(t, func() bool { return true })
	base := trustedBaseDir(t)
	m := adoptMgr(base)
	st, err := m.StartSession("")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := loadRecord(m.registryDir(), st.ID)
	if err != nil || rec.Egress != EgressPublicOnly {
		t.Fatalf("record egress = %q (%v)", rec.Egress, err)
	}
	// A new manager over the same dirs must not take the browser over.
	m2 := adoptMgr(base)
	if _, ok := m2.SessionStatus(st.ID); ok {
		t.Fatal("public-only session was re-adopted without its guard")
	}
	rec2, err := loadRecord(m2.registryDir(), st.ID)
	if err != nil || rec2.State != RecordExpired {
		t.Fatalf("unadopted record should be expired: %+v %v", rec2, err)
	}
}

func TestEgressSessionStoppedBeforeSelfRestart(t *testing.T) {
	newAdoptEnv(t) // adoption supported
	e := newEgressEnv(t)
	m := adoptMgr(trustedBaseDir(t))
	on := true
	m.SetEgressRequired(func() bool { return on })
	guarded, _ := m.StartSession("")
	on = false
	plain, _ := m.StartSession("")

	m.prepareSelfRestart(EndedReasonNodeUpdate, "to v9")

	if _, ok := m.SessionStatus(guarded.ID); ok {
		t.Fatal("public-only session must be stopped before a self-restart")
	}
	if e2, ok := m.EndedSession(guarded.ID); !ok || e2.Reason != "node_update" {
		t.Fatalf("ended = %+v %v", e2, ok)
	}
	if guardListening(e.guards[0]) {
		t.Fatal("guard still listening")
	}
	if _, ok := m.SessionStatus(plain.ID); !ok {
		t.Fatal("an unguarded session should stay for re-adoption")
	}
}

func TestEgressAuditLogsOncePerDestinationAndCaps(t *testing.T) {
	a := newEgressAudit("cb-0123456789abcdef")
	for i := 0; i < 3; i++ {
		a.refused("http", "10.0.0.1")
	}
	if len(a.seen) != 1 {
		t.Fatalf("seen = %d", len(a.seen))
	}
	for i := 0; i < egressAuditMaxHosts+10; i++ {
		a.refused("https", "h"+strconv.Itoa(i)+".test")
	}
	if len(a.seen) != egressAuditMaxHosts || !a.capped {
		t.Fatalf("seen = %d capped = %v", len(a.seen), a.capped)
	}
}

// TestLegacyCobrowseRefusedWhenPublicOnly: the legacy single browser has no
// guard, so a public-only node refuses to start or navigate it.
func TestLegacyCobrowseRefusedWhenPublicOnly(t *testing.T) {
	setNodeEgress(t, func() bool { return true })
	m := &CobrowseManager{driver: DriverAI, debugPort: DefaultCobrowseDebugPort}
	if _, err := m.Start(t.TempDir(), "https://a.example", 0); !errors.Is(err, ErrLegacyCobrowsePublicOnly) {
		t.Fatalf("start: want public-only refusal, got %v", err)
	}
	if _, err := m.Navigate("https://a.example"); !errors.Is(err, ErrLegacyCobrowsePublicOnly) {
		t.Fatalf("navigate: want public-only refusal, got %v", err)
	}
	setNodeEgress(t, func() bool { return false })
	if _, err := m.Navigate("https://a.example"); errors.Is(err, ErrLegacyCobrowsePublicOnly) {
		t.Fatal("refused with public-only off")
	}
}

// TestEgressRouteCheckFailureRefusesSession: a browser that does not send the
// canary through its guard (a managed proxy policy) is stopped and refused.
func TestEgressRouteCheckFailureRefusesSession(t *testing.T) {
	e := newEgressEnv(t)
	e.routeErr = errEgressNotRouted
	setNodeEgress(t, func() bool { return true })
	m := adoptMgr(trustedBaseDir(t))
	if _, err := m.StartSession(""); !errors.Is(err, errEgressNotRouted) {
		t.Fatalf("want a route refusal, got %v", err)
	}
	if e.routed != 1 || e.stops != 1 {
		t.Fatalf("route checks = %d, browser stops = %d; want 1 and 1", e.routed, e.stops)
	}
	if guardListening(e.guards[0]) {
		t.Fatal("guard still listening after a refused start")
	}
	if n := len(m.List()); n != 0 {
		t.Fatalf("%d session(s) left", n)
	}
	// Unguarded sessions are never route-checked.
	setNodeEgress(t, func() bool { return false })
	if _, err := m.StartSession(""); err != nil {
		t.Fatal(err)
	}
	defer m.StopAll()
	if e.routed != 1 {
		t.Fatalf("an unguarded session was route-checked")
	}
}

// TestWatchGuardStopsSessionOnGuardFailure: a guard that stops serving on its
// own ends the session; a normal stop does not trigger it.
func TestWatchGuardStopsSessionOnGuardFailure(t *testing.T) {
	newEgressEnv(t)
	m := adoptMgr(trustedBaseDir(t))
	m.SetEgressRequired(func() bool { return true })
	st, err := m.StartSession("")
	if err != nil {
		t.Fatal(err)
	}
	failed, stopped := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() { m.watchGuard(st.ID, failed, stopped); close(done) }()
	close(failed)
	<-done
	if _, ok := m.SessionStatus(st.ID); ok {
		t.Fatal("session kept running after its guard failed")
	}

	st2, _ := m.StartSession("")
	defer m.StopAll()
	failed2, stopped2 := make(chan struct{}), make(chan struct{})
	done2 := make(chan struct{})
	go func() { m.watchGuard(st2.ID, failed2, stopped2); close(done2) }()
	close(stopped2)
	<-done2
	if _, ok := m.SessionStatus(st2.ID); !ok {
		t.Fatal("a normally stopped guard must not stop another path's session")
	}
}

// TestLegacyCobrowseStoppedWhenPublicOnlyTurnsOn: a legacy browser already
// running when the setting turns on is stopped at its next action.
func TestLegacyCobrowseStoppedWhenPublicOnlyTurnsOn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX sleep as a stand-in process")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}
	m := &CobrowseManager{driver: DriverAI, debugPort: DefaultCobrowseDebugPort}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	m.cmd, m.exited = cmd, exited
	go func() { _ = cmd.Wait(); close(exited) }()
	defer m.Stop()
	if !m.IsRunning() {
		t.Fatal("stand-in not running")
	}
	setNodeEgress(t, func() bool { return true })
	if _, err := m.Handoff(); !errors.Is(err, ErrLegacyCobrowsePublicOnly) {
		t.Fatalf("want public-only refusal, got %v", err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the legacy browser kept running after public-only turned on")
	}
}
