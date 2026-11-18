package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/browserruntime"
)

const (
	testImage = "ghcr.io/aceteam-ai/browser-runtime:abc@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	testCID   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testSID   = "cb-0123456789abcdef"
)

type fakeEngine struct {
	mu        sync.Mutex
	calls     [][]string
	failOn    string // first arg (or "network create") that errors
	members   string // output of ps --filter network=
	networks  string // inspect: networks json
	psOut     string
	netLsOut  string
	inContext string // in-container ws
	running   bool
}

func (f *fakeEngine) exec(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{}, args...))
	key := args[0]
	if key == "network" || key == "image" {
		key += " " + args[1]
	}
	if f.failOn == key {
		return "", errors.New("boom")
	}
	switch key {
	case "info":
		return "id\n", nil
	case "image inspect":
		return browserruntime.ContractVersion + "\n", nil
	case "network create":
		return "netid\n", nil
	case "run":
		return testCID[:12] + "\n", nil
	case "inspect":
		nets := f.networks
		if nets == "" {
			nets = `{"` + browserruntime.NetworkName(testSID) + `":{}}`
		}
		return fmt.Sprintf(`{"Id":%q,"State":{"Running":%v,"StartedAt":"t0"},"Config":{"Labels":{%q:"1",%q:%q}},"NetworkSettings":{"Networks":%s}}`,
			testCID, f.running || true, browserruntime.LabelRuntime, browserruntime.LabelSession, testSID, nets), nil
	case "port":
		if strings.HasPrefix(args[2], "8102") {
			return "127.0.0.1:41000\n", nil
		}
		return "127.0.0.1:41001\n", nil
	case "ps":
		for _, a := range args {
			if strings.HasPrefix(a, "network=") {
				return f.members, nil
			}
		}
		return f.psOut, nil
	case "exec":
		return `{"webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/browser/` + f.inContext + `"}`, nil
	case "network ls":
		return f.netLsOut, nil
	}
	return "", nil
}

func (f *fakeEngine) has(prefix ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if len(c) >= len(prefix) && strings.Join(c[:len(prefix)], " ") == strings.Join(prefix, " ") {
			n++
		}
	}
	return n
}

func (f *fakeEngine) index(prefix ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.calls {
		if len(c) >= len(prefix) && strings.Join(c[:len(prefix)], " ") == strings.Join(prefix, " ") {
			return i
		}
	}
	return -1
}

func newTestRunner(f *fakeEngine, podman bool) *containerRunner {
	r := &containerRunner{
		eng: f.exec, podman: podman, image: testImage, uid: 1000, gid: 1000,
		limits: ContainerLimits{DiskCacheMB: 5}, now: time.Now, pollEvery: time.Hour,
		startBrowser:  func(context.Context, int, string) error { return nil },
		chromeRunning: func(context.Context, int, string) bool { return true },
		cdpReady:      func(context.Context, int) error { return nil },
		browserIDAt:   func(context.Context, int) (string, error) { return "BID", nil },
		navigate:      func(context.Context, int, string) error { return nil },
	}
	f.inContext = "BID"
	return r
}

func profileFor(t *testing.T) string {
	d := filepath.Join(t.TempDir(), testSID)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestContainerLaunchOrderAndContract(t *testing.T) {
	f := &fakeEngine{}
	r := newTestRunner(f, true)
	p, err := r.Launch(profileFor(t), "https://example.test/", LaunchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.stop()
	if p.debugPort != 41001 || p.containerCtrl != 41000 || p.containerID != testCID || p.browserID != "BID" {
		t.Fatalf("proc %+v", p)
	}
	if f.index("network", "create") > f.index("run") || f.index("network", "create") < 0 {
		t.Fatal("network must be created before the container")
	}
	var netArgs, runArgs []string
	for _, c := range f.calls {
		if c[0] == "network" && c[1] == "create" {
			netArgs = c
		}
		if c[0] == "run" {
			runArgs = c
		}
	}
	j := strings.Join(netArgs, " ")
	if !strings.Contains(j, "--driver bridge") || !strings.Contains(j, "isolate=strict") || !strings.HasSuffix(j, browserruntime.NetworkName(testSID)) {
		t.Fatalf("network args %v", netArgs)
	}
	rj := strings.Join(runArgs, " ")
	for _, want := range []string{"--pull=never", "--userns=keep-id", "--cap-drop ALL", "--read-only", "MEETING_DISK_CACHE_MB=5", "--name " + containerName(testSID), "--network " + browserruntime.NetworkName(testSID)} {
		if !strings.Contains(rj, want) {
			t.Errorf("run args missing %q: %v", want, runArgs)
		}
	}
	if runArgs[len(runArgs)-1] != testImage {
		t.Fatal("image must be last")
	}
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
	if f.has("rm", "-f", testCID) != 1 || f.has("network", "rm", browserruntime.NetworkName(testSID)) != 1 {
		t.Fatalf("teardown must remove container and network: %v", f.calls)
	}
}

func TestContainerDockerHasNoPodmanFlags(t *testing.T) {
	f := &fakeEngine{}
	r := newTestRunner(f, false)
	p, err := r.Launch(profileFor(t), "", LaunchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.stop()
	for _, c := range f.calls {
		for _, a := range c {
			if a == "--userns=keep-id" || a == "isolate=strict" {
				t.Fatalf("podman-only flag on docker: %v", c)
			}
		}
	}
}

func TestContainerLaunchFailuresCleanUp(t *testing.T) {
	cases := map[string]func(*fakeEngine, *containerRunner){
		"run": func(f *fakeEngine, r *containerRunner) { f.failOn = "run" },
		"foreign member": func(f *fakeEngine, r *containerRunner) {
			f.members = testCID + "\n" + strings.Repeat("b", 64) + "\n"
		},
		"second network": func(f *fakeEngine, r *containerRunner) {
			f.networks = `{"` + browserruntime.NetworkName(testSID) + `":{},"bridge":{}}`
		},
		"impostor": func(f *fakeEngine, r *containerRunner) { f.inContext = "OTHER" },
		"cdp": func(f *fakeEngine, r *containerRunner) {
			r.cdpReady = func(context.Context, int) error { return errors.New("no cdp") }
		},
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeEngine{}
			r := newTestRunner(f, false)
			mod(f, r)
			if _, err := r.Launch(profileFor(t), "", LaunchOptions{}); err == nil {
				t.Fatal("expected failure")
			}
			if f.has("network", "rm", browserruntime.NetworkName(testSID)) != 1 {
				t.Fatalf("network leaked: %v", f.calls)
			}
			if name != "run" && f.has("rm", "-f") != 1 {
				t.Fatalf("container leaked: %v", f.calls)
			}
		})
	}
}

func TestContainerNetworkAlreadyExistsIsFailure(t *testing.T) {
	f := &fakeEngine{failOn: "network create"}
	r := newTestRunner(f, false)
	if _, err := r.Launch(profileFor(t), "", LaunchOptions{}); err == nil {
		t.Fatal("expected failure")
	}
	if f.has("run") != 0 || f.has("network", "rm") != 0 {
		t.Fatal("must not run or remove a network we did not create")
	}
}

func TestContainerHealthFailsClosed(t *testing.T) {
	f := &fakeEngine{failOn: "info"}
	r := newTestRunner(f, false)
	if err := r.healthy(true); err == nil {
		t.Fatal("unreachable engine must be unhealthy")
	}
	r2 := newTestRunner(&fakeEngine{}, false)
	r2.image = ""
	if r2.healthy(true) == nil {
		t.Fatal("no pinned image must be unhealthy")
	}
	r3 := newTestRunner(&fakeEngine{}, false)
	r3.uid = 0
	if r3.healthy(true) == nil {
		t.Fatal("root uid must be unhealthy")
	}
	f4 := &fakeEngine{failOn: "image inspect"}
	r4 := newTestRunner(f4, false)
	f4.failOn = "image inspect"
	if r4.healthy(false) != nil {
		t.Fatal("absent but pullable image counts as available for discovery")
	}
}

func TestContainerSweepRemovesOnlyDeadSessions(t *testing.T) {
	dead := strings.Repeat("c", 64)
	f := &fakeEngine{
		psOut:    testCID + " " + testSID + "\n" + dead + " cb-ffffffffffffffff\n",
		netLsOut: browserruntime.NetworkName(testSID) + "\n" + browserruntime.NetworkName("cb-ffffffffffffffff") + "\nbridge\n",
	}
	r := newTestRunner(f, false)
	r.sweepContainers(func(id string) bool { return id == testSID })
	if f.has("rm", "-f", dead) != 1 || f.has("rm", "-f", testCID) != 0 {
		t.Fatalf("container sweep: %v", f.calls)
	}
	if f.has("network", "rm", browserruntime.NetworkName("cb-ffffffffffffffff")) != 1 ||
		f.has("network", "rm", browserruntime.NetworkName(testSID)) != 0 || f.has("network", "rm", "bridge") != 0 {
		t.Fatalf("network sweep: %v", f.calls)
	}
}

func goodRecord() sessionRecord {
	return sessionRecord{Version: sessionRecordVersion, ID: testSID, Runner: "container", ContainerID: testCID,
		ContainerStart: "t0", DebugPort: 41001, ControlPort: 41000, BrowserID: "BID"}
}

func TestContainerAdoptVerifiesEverything(t *testing.T) {
	f := &fakeEngine{}
	r := newTestRunner(f, false)
	p, err := r.adoptContainer(goodRecord(), "/x/"+testSID)
	if err != nil {
		t.Fatal(err)
	}
	p.stop()
	bad := map[string]func(*sessionRecord){
		"short id":      func(s *sessionRecord) { s.ContainerID = "abc" },
		"restarted":     func(s *sessionRecord) { s.ContainerStart = "t1" },
		"cdp port":      func(s *sessionRecord) { s.DebugPort = 1 },
		"control port":  func(s *sessionRecord) { s.ControlPort = 1 },
		"browser id":    func(s *sessionRecord) { s.BrowserID = "OTHER" },
		"no browser id": func(s *sessionRecord) { s.BrowserID = "" },
		"wrong session": func(s *sessionRecord) { s.ID = "cb-ffffffffffffffff" },
	}
	for name, mod := range bad {
		t.Run(name, func(t *testing.T) {
			rec := goodRecord()
			mod(&rec)
			if _, err := newTestRunner(&fakeEngine{}, false).adoptContainer(rec, "/x/"+rec.ID); err == nil {
				t.Fatal("record that fails verification must not be adopted")
			}
		})
	}
	t.Run("foreign member", func(t *testing.T) {
		f := &fakeEngine{members: strings.Repeat("d", 64)}
		if _, err := newTestRunner(f, false).adoptContainer(goodRecord(), "/x/"+testSID); err == nil {
			t.Fatal("expected failure")
		}
	})
}

func TestDiscardOnlyRemovesLabelledContainer(t *testing.T) {
	f := &fakeEngine{}
	r := newTestRunner(f, false)
	rec := goodRecord()
	rec.ID = "cb-ffffffffffffffff" // labels say testSID: not ours to remove
	r.discardContainer(rec)
	if f.has("rm", "-f") != 0 {
		t.Fatal("must not remove a container whose labels name another session")
	}
	r.discardContainer(goodRecord())
	if f.has("rm", "-f", testCID) != 1 {
		t.Fatal("expected removal")
	}
}

type markerRunner struct {
	name     string
	launched *int
}

func (m markerRunner) Name() string                   { return m.name }
func (m markerRunner) Discover() (BrowserInfo, error) { return BrowserInfo{Path: m.name}, nil }
func (m markerRunner) PrepareBaseDir(string) error    { return nil }
func (m markerRunner) SweepOrphans(string)            {}
func (m markerRunner) Launch(string, string, LaunchOptions) (*cobrowseProc, error) {
	*m.launched++
	return &cobrowseProc{}, nil
}

func TestSelectingRunnerFallsBackToHost(t *testing.T) {
	hostN := 0
	f := &fakeEngine{failOn: "info"}
	s := &selectingRunner{host: markerRunner{"host", &hostN}, container: newTestRunner(f, false)}
	if _, err := s.Launch(profileFor(t), "", LaunchOptions{NodeWide: true}); err != nil || hostN != 1 {
		t.Fatalf("unhealthy runtime must fall back to host: %v %d", err, hostN)
	}
	if s.Name() != "host" {
		t.Fatal("name")
	}
	// Local mode never uses containers, even when healthy.
	f2 := &fakeEngine{}
	s2 := &selectingRunner{host: markerRunner{"host", &hostN}, container: newTestRunner(f2, false)}
	if _, err := s2.Launch(profileFor(t), "", LaunchOptions{}); err != nil || hostN != 2 || f2.has("run") != 0 {
		t.Fatal("non-node-wide launch must use the host runner")
	}
	// No container configured: pure host.
	s3 := &selectingRunner{host: markerRunner{"host", &hostN}}
	if _, err := s3.Launch(profileFor(t), "", LaunchOptions{NodeWide: true}); err != nil || hostN != 3 {
		t.Fatal("unconfigured runtime must use host")
	}
}

func TestSelectingRunnerUsesContainerWhenHealthy(t *testing.T) {
	hostN := 0
	f := &fakeEngine{}
	s := &selectingRunner{host: markerRunner{"host", &hostN}, container: newTestRunner(f, false)}
	p, err := s.Launch(profileFor(t), "", LaunchOptions{NodeWide: true})
	if err != nil || hostN != 0 {
		t.Fatalf("%v %d", err, hostN)
	}
	p.stop()
	if s.Name() != "container" {
		t.Fatal("name")
	}
}

func TestPruneProfileCaches(t *testing.T) {
	d := t.TempDir()
	cache := filepath.Join(d, "Default", "Cache")
	os.MkdirAll(cache, 0o700)
	os.WriteFile(filepath.Join(cache, "f"), make([]byte, 2048), 0o600)
	os.WriteFile(filepath.Join(d, "Cookies"), []byte("keep"), 0o600)
	pruneProfileCaches(d, 4096)
	if _, err := os.Stat(cache); err != nil {
		t.Fatal("under the bound must be kept")
	}
	pruneProfileCaches(d, 1024)
	if _, err := os.Stat(cache); err == nil {
		t.Fatal("over the bound must be removed")
	}
	if _, err := os.Stat(filepath.Join(d, "Cookies")); err != nil {
		t.Fatal("profile data must survive")
	}
}

func TestBrowserFilePathMapsContainerMount(t *testing.T) {
	p := &cobrowseProc{containerMount: "/h/cb-1"}
	if got := p.browserFilePath("/h/cb-1/.citadel-stage/a.txt"); got != "/profile/.citadel-stage/a.txt" {
		t.Fatal(got)
	}
	if got := p.browserFilePath("/elsewhere/a"); got != "/elsewhere/a" {
		t.Fatal(got)
	}
	if got := (&cobrowseProc{}).browserFilePath("/h/a"); got != "/h/a" {
		t.Fatal(got)
	}
}

func TestSetBrowserSlotsRespectsEnv(t *testing.T) {
	t.Cleanup(func() { configuredBrowserSlots.Store(0) })
	t.Setenv(EnvCobrowseMaxSessions, "")
	configuredBrowserSlots.Store(5)
	if resolveMaxCobrowseSessions() != 5 {
		t.Fatal("slots")
	}
	t.Setenv(EnvCobrowseMaxSessions, "3")
	if resolveMaxCobrowseSessions() != 3 {
		t.Fatal("env must win")
	}
}

func TestRewriteBrowserWSIsNoopForHostEndpoint(t *testing.T) {
	got, err := rewriteLoopbackWSPort("ws://127.0.0.1:9222/devtools/browser/X", 41001)
	if err != nil || got != "ws://127.0.0.1:41001/devtools/browser/X" {
		t.Fatal(got, err)
	}
}
