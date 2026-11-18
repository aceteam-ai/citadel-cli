package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
	mu             sync.Mutex
	calls          [][]string
	failOn         string // first arg (or "network create") that errors
	members        string // output of ps --filter network=
	networks       string // inspect: networks json
	psOut          string
	netLsOut       string
	inContext      string // in-container ws
	notRunning     bool
	owner          string // owner label the fake containers carry
	imageLabels    string // image inspect output; default "1|1"
	inspectFail    bool   // inspect errors
	runOut         string // output returned together with a failing run
	foreignPurpose bool   // containers carry purpose=meeting
	foreignNet     bool   // networks carry another owner
}

func (f *fakeEngine) exec(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{}, args...))
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, browserruntime.LabelOwner+"="); ok && f.owner == "" {
			f.owner = v
		}
	}
	key := args[0]
	if key == "network" || key == "image" {
		key += " " + args[1]
	}
	if f.failOn == key {
		if key == "run" && f.runOut != "" {
			return f.runOut, errors.New("boom")
		}
		return "", errors.New("boom")
	}
	switch key {
	case "info":
		return "id\n", nil
	case "image inspect":
		if f.imageLabels != "" {
			return f.imageLabels + "\n", nil
		}
		return browserruntime.ContractVersion + "|" + browserruntime.SandboxMarker + "\n", nil
	case "network create":
		return "netid\n", nil
	case "run":
		return testCID[:12] + "\n", nil
	case "inspect":
		if f.inspectFail {
			return "", errors.New("inspect boom")
		}
		nets := f.networks
		if nets == "" {
			nets = `{"` + browserruntime.NetworkName(testSID) + `":{}}`
		}
		return fmt.Sprintf(`{"Id":%q,"State":{"Running":%v,"StartedAt":"t0"},"Config":{"Labels":{%q:"1",%q:%q,%q:%q,%q:%q}},"NetworkSettings":{"Networks":%s}}`,
			testCID, !f.notRunning, browserruntime.LabelRuntime, browserruntime.LabelSession, testSID, browserruntime.LabelOwner, f.owner, browserruntime.LabelPurpose, f.purpose(), nets), nil
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
	case "network inspect":
		name := args[len(args)-1]
		owner := f.owner
		if f.foreignNet {
			owner = "someoneelse"
		}
		return fmt.Sprintf(`{%q:"1",%q:%q,%q:"cobrowse",%q:%q}`, browserruntime.LabelRuntime,
			browserruntime.LabelSession, strings.TrimPrefix(name, browserruntime.NetworkName("")),
			browserruntime.LabelPurpose, browserruntime.LabelOwner, owner), nil
	case "network ls":
		return f.netLsOut, nil
	}
	return "", nil
}

func (f *fakeEngine) purpose() string {
	if f.foreignPurpose {
		return "meeting"
	}
	return "cobrowse"
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
	r.uid, r.gid = 1000, 1000
	return r
}

// profileFor returns a session dir (<tmp>/cb-...); the runner's owner is
// derived from its parent, so tests that launch re-derive it with ownerFor.
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
	sess := profileFor(t)
	p, err := r.Launch(sess, "https://example.test/", LaunchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.stop()
	if p.containerStage != filepath.Join(sess, cobrowseStagingDirName) {
		t.Fatalf("stage dir %q", p.containerStage)
	}
	if fi, err := os.Lstat(filepath.Join(sess, containerProfileSubdir)); err != nil || !fi.IsDir() {
		t.Fatal("profile subdir must exist")
	}
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
	// Only the profile subdir is mounted rw; the session dir itself never is.
	if strings.Contains(rj, "src="+sess+",") {
		t.Fatalf("session dir must not be mounted: %v", runArgs)
	}
	if !strings.Contains(rj, "src="+filepath.Join(sess, containerProfileSubdir)+",dst=/profile") {
		t.Fatalf("profile subdir mount missing: %v", runArgs)
	}
	for _, want := range []string{"--pull=never", "--userns=keep-id", "--cap-drop ALL", ",dst=/profile", "dst=" + browserruntime.StageMountPath + ",readonly", browserruntime.LabelOwner + "=", "--read-only", "MEETING_DISK_CACHE_MB=5", "--name " + containerName(testSID), "--network " + browserruntime.NetworkName(testSID)} {
		if !strings.Contains(rj, want) {
			t.Errorf("run args missing %q: %v", want, runArgs)
		}
	}
	if runArgs[len(runArgs)-1] != testImage {
		t.Fatal("image must be last")
	}
	// The pinned seccomp profile is written into the session dir and handed to
	// the engine; the renderer sandbox cannot start without it (#1788).
	seccompFile := filepath.Join(sess, browserruntime.SeccompFileName)
	if !strings.Contains(rj, "--security-opt seccomp="+seccompFile) {
		t.Errorf("run args must carry the pinned seccomp profile: %v", runArgs)
	}
	if b, err := os.ReadFile(seccompFile); err != nil || string(b) != string(browserruntime.SeccompProfileJSON) {
		t.Errorf("seccomp file must hold the pinned profile (err=%v)", err)
	}
	if !strings.Contains(rj, "MEETING_BROWSER=chromium") {
		t.Errorf("browser kind missing: %v", runArgs)
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
		"run":               func(f *fakeEngine, r *containerRunner) { f.failOn = "run" },
		"inspect after run": func(f *fakeEngine, r *containerRunner) { f.inspectFail = true },
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
			if f.has("rm", "-f") != 1 {
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
	// A contract-1 image without the sandbox marker is unusable for co-browse.
	for _, labels := range []string{"1|", "1|0", "2|1", "|"} {
		rs := newTestRunner(&fakeEngine{imageLabels: labels}, false)
		if rs.healthy(true) == nil {
			t.Fatalf("image labels %q must be unhealthy", labels)
		}
	}
	if newTestRunner(&fakeEngine{imageLabels: "1|1"}, false).healthy(true) != nil {
		t.Fatal("sandboxed image must be healthy")
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

func TestContainerSweepScopedAndShapeChecked(t *testing.T) {
	dead := strings.Repeat("c", 64)
	other := strings.Repeat("e", 64)
	f := &fakeEngine{
		psOut: testCID + " " + containerName(testSID) + " " + testSID + "\n" + // live
			dead + " " + containerName("cb-ffffffffffffffff") + " cb-ffffffffffffffff\n" + // dead, ours
			other + " someone-elses-name cb-eeeeeeeeeeeeeeee\n" + // wrong name: skipped
			other + " " + containerName("evil") + " evil\n" + // malformed session label: skipped
			"short x y\n", // malformed id: skipped
		netLsOut: browserruntime.NetworkName(testSID) + "\n" + browserruntime.NetworkName("cb-ffffffffffffffff") + "\n" +
			browserruntime.NetworkName("evil") + "\nbridge\n",
	}
	r := newTestRunner(f, false)
	f.owner = r.ownerFor("/x")
	r.sweepContainers("/x", func(id string) bool { return id == testSID })
	if f.has("rm", "-f", dead) != 1 || f.has("rm", "-f", testCID) != 0 || f.has("rm", "-f", other) != 0 {
		t.Fatalf("container sweep: %v", f.calls)
	}
	if f.has("network", "rm", browserruntime.NetworkName("cb-ffffffffffffffff")) != 1 ||
		f.has("network", "rm", browserruntime.NetworkName(testSID)) != 0 ||
		f.has("network", "rm", browserruntime.NetworkName("evil")) != 0 || f.has("network", "rm", "bridge") != 0 {
		t.Fatalf("network sweep: %v", f.calls)
	}
	// The listing itself is scoped to this worker (owner) and the cobrowse purpose,
	// so a foreign owner or a meeting container is never even returned.
	for _, c := range f.calls {
		if c[0] == "ps" || (c[0] == "network" && c[1] == "ls") {
			j := strings.Join(c, " ")
			for _, want := range []string{"label=" + browserruntime.LabelOwner + "=" + r.ownerFor("/x"), "label=" + browserruntime.LabelPurpose + "=cobrowse"} {
				if !strings.Contains(j, want) {
					t.Errorf("listing %v lacks filter %q", c, want)
				}
			}
		}
	}
	if r.ownerFor("/x") == r.ownerFor("/y") {
		t.Fatal("owner must depend on the base dir")
	}
}

func goodRecord() sessionRecord {
	return sessionRecord{Version: sessionRecordVersion, ID: testSID, Runner: "container", ContainerID: testCID,
		ContainerStart: "t0", DebugPort: 41001, ControlPort: 41000, BrowserID: "BID"}
}

func adoptRunner(f *fakeEngine) *containerRunner {
	r := newTestRunner(f, false)
	f.owner = r.ownerFor("/x")
	return r
}

func TestContainerAdoptVerifiesEverything(t *testing.T) {
	f := &fakeEngine{}
	r := adoptRunner(f)
	p, err := r.adoptContainer(goodRecord(), "/x/"+testSID)
	if err != nil {
		t.Fatal(err)
	}
	p.stop()
	bad := map[string]func(*sessionRecord, *fakeEngine){
		"short id":       func(s *sessionRecord, _ *fakeEngine) { s.ContainerID = "abc" },
		"restarted":      func(s *sessionRecord, _ *fakeEngine) { s.ContainerStart = "t1" },
		"cdp port":       func(s *sessionRecord, _ *fakeEngine) { s.DebugPort = 1 },
		"control port":   func(s *sessionRecord, _ *fakeEngine) { s.ControlPort = 1 },
		"browser id":     func(s *sessionRecord, _ *fakeEngine) { s.BrowserID = "OTHER" },
		"no browser id":  func(s *sessionRecord, _ *fakeEngine) { s.BrowserID = "" },
		"wrong session":  func(s *sessionRecord, _ *fakeEngine) { s.ID = "cb-ffffffffffffffff" },
		"not running":    func(_ *sessionRecord, f *fakeEngine) { f.notRunning = true },
		"foreign owner":  func(_ *sessionRecord, f *fakeEngine) { f.owner = "someoneelse" },
		"foreign member": func(_ *sessionRecord, f *fakeEngine) { f.members = strings.Repeat("d", 64) },
	}
	for name, mod := range bad {
		t.Run(name, func(t *testing.T) {
			f := &fakeEngine{}
			r := adoptRunner(f)
			rec := goodRecord()
			mod(&rec, f)
			if _, err := r.adoptContainer(rec, "/x/"+rec.ID); err == nil {
				t.Fatal("record that fails verification must not be adopted")
			}
		})
	}
}

func TestDiscardOnlyRemovesLabelledContainer(t *testing.T) {
	f := &fakeEngine{}
	r := adoptRunner(f)
	rec := goodRecord()
	rec.ID = "cb-ffffffffffffffff" // labels say testSID: not ours to remove
	r.discardContainer(rec, "/x/"+rec.ID)
	if f.has("rm", "-f") != 0 {
		t.Fatal("must not remove a container whose labels name another session")
	}
	r.discardContainer(goodRecord(), "/x/"+testSID)
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

func TestSelectingRunnerDecision(t *testing.T) {
	hostN := 0
	// Unhealthy runtime: the manager's decision is host.
	s := &selectingRunner{host: markerRunner{"host", &hostN}, container: newTestRunner(&fakeEngine{failOn: "info"}, false)}
	if s.useContainer(true) {
		t.Fatal("unhealthy runtime must not be chosen")
	}
	if s.Name() != "host" {
		t.Fatal("name")
	}
	if _, err := s.Launch(profileFor(t), "", LaunchOptions{}); err != nil || hostN != 1 {
		t.Fatalf("host launch: %v %d", err, hostN)
	}
	// Healthy, but local mode (not node-wide) never uses containers.
	f2 := &fakeEngine{}
	s2 := &selectingRunner{host: markerRunner{"host", &hostN}, container: newTestRunner(f2, false)}
	if s2.useContainer(false) || !s2.useContainer(true) {
		t.Fatal("only the node-wide manager may use a healthy runtime")
	}
	// Sandbox-less image: unhealthy for co-browse, falls back.
	s4 := &selectingRunner{host: markerRunner{"host", &hostN}, container: newTestRunner(&fakeEngine{imageLabels: "1|"}, false)}
	if s4.useContainer(true) {
		t.Fatal("image without the sandbox marker must fall back to host")
	}
	// No container configured: pure host.
	s3 := &selectingRunner{host: markerRunner{"host", &hostN}}
	if s3.useContainer(true) {
		t.Fatal("unconfigured runtime must use host")
	}
	if _, err := s3.Launch(profileFor(t), "", LaunchOptions{UseContainer: true}); err == nil {
		t.Fatal("a container launch with no runtime must fail, not silently use the host")
	}
}

func TestSelectingRunnerUsesContainerWhenHealthy(t *testing.T) {
	hostN := 0
	f := &fakeEngine{}
	s := &selectingRunner{host: markerRunner{"host", &hostN}, container: newTestRunner(f, false)}
	p, err := s.Launch(profileFor(t), "", LaunchOptions{UseContainer: true})
	if err != nil || hostN != 0 {
		t.Fatalf("%v %d", err, hostN)
	}
	p.stop()
	if s.Name() != "container" {
		t.Fatal("name")
	}
}

func TestContainerIdentityHasUsableNonRootIDs(t *testing.T) {
	uid, gid := containerIdentity()
	if runtime.GOOS == "windows" && (uid != windowsContainerID || gid != windowsContainerID) {
		t.Fatal("windows must map to the image user")
	}
	if uid < 0 || gid < 0 {
		t.Fatal("never negative")
	}
	// Windows host values (-1) must not make the runtime unhealthy.
	r := newTestRunner(&fakeEngine{}, false)
	r.uid, r.gid = windowsContainerID, windowsContainerID
	if err := r.healthy(true); err != nil {
		t.Fatal(err)
	}
}

func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{max: 8}
	n, err := b.Write([]byte("0123456789"))
	if n != 10 || err != nil || b.String() != "01234567" {
		t.Fatal(n, err, b.String())
	}
	b.Write([]byte("more"))
	if b.Len() != 8 {
		t.Fatal("cap exceeded")
	}
}

func TestFailedRunWithDaemonCreatedContainerIsReconciledByName(t *testing.T) {
	// `run` errors (client timeout) but the daemon started it: no id was
	// reported, so the container is found by its deterministic name.
	f := &fakeEngine{failOn: "run"}
	r := newTestRunner(f, false)
	if _, err := r.Launch(profileFor(t), "", LaunchOptions{}); err == nil {
		t.Fatal("expected failure")
	}
	if f.has("rm", "-f", testCID) != 1 {
		t.Fatalf("container created by an erroring run must be removed: %v", f.calls)
	}
	// A same-named container that is not ours (labels differ) is left alone.
	f2 := &fakeEngine{failOn: "run", owner: "someoneelse"}
	r2 := newTestRunner(f2, false)
	if _, err := r2.Launch(profileFor(t), "", LaunchOptions{}); err == nil {
		t.Fatal("expected failure")
	}
	if f2.has("rm", "-f") != 0 {
		t.Fatalf("foreign container must not be removed: %v", f2.calls)
	}
}

func TestFailedLaunchKeepsReportedIDWhenInspectFails(t *testing.T) {
	f := &fakeEngine{inspectFail: true}
	r := newTestRunner(f, false)
	if _, err := r.Launch(profileFor(t), "", LaunchOptions{}); err == nil {
		t.Fatal("expected failure")
	}
	if f.has("rm", "-f", testCID[:12]) != 1 {
		t.Fatalf("the id run reported must be removed: %v", f.calls)
	}
}

// ---- pidfile / staging symlink attacks ----

func TestPidfileNeverWritesThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(victim, []byte("keep"), 0o600)
	if err := os.Symlink(victim, filepath.Join(dir, sessionPidfileName)); err != nil {
		t.Skip("symlinks unavailable")
	}
	writeSessionPidfile(dir, 1, 2, 3, 0)
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("victim overwritten through the symlink: %q", b)
	}
	fi, err := os.Lstat(filepath.Join(dir, sessionPidfileName))
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("pidfile must be a regular file now: %v %v", fi, err)
	}
}

func TestStagingDirSwappedToSymlinkIsRefused(t *testing.T) {
	sess := &cobrowseSession{id: testSID, profile: t.TempDir()}
	target := t.TempDir()
	if err := os.Symlink(target, sess.stagingDirPath()); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := sess.ensureStagingDir(); err == nil {
		t.Fatal("a symlinked staging dir must be refused")
	}
	if _, err := sess.existingStagingDir(); err == nil {
		t.Fatal("a symlinked staging dir must be refused for reads too")
	}
	// The container runner refuses to mount one as well.
	f := &fakeEngine{}
	r := newTestRunner(f, false)
	sessDir := profileFor(t)
	os.Symlink(target, filepath.Join(sessDir, cobrowseStagingDirName))
	if _, err := r.Launch(sessDir, "", LaunchOptions{}); err == nil || f.has("run") != 0 {
		t.Fatal("launch must refuse a symlinked staging dir before running anything")
	}
	// ...and a symlinked profile subdir.
	sessDir2 := profileFor(t)
	os.Symlink(target, filepath.Join(sessDir2, containerProfileSubdir))
	if _, err := r.Launch(sessDir2, "", LaunchOptions{}); err == nil || f.has("run") != 0 {
		t.Fatal("launch must refuse a symlinked profile dir")
	}
}

// ---- manager-level: recover and shutdown ----

func containerManager(t *testing.T, f *fakeEngine) (*CobrowseSessionManager, string, func()) {
	base := t.TempDir()
	os.Chmod(base, 0o700)
	m := newCobrowseSessionManager(base, 4)
	m.useResolver = true
	regDirResolverPrev := registryDirResolver
	reg := t.TempDir()
	os.Chmod(reg, 0o700)
	SetRegistryDirResolver(func() string { return reg })
	prev, prevAdopt, prevDet := activeBrowserRunner, adoptionSupported, detachedWorker.Load()
	r := newTestRunner(f, false)
	f.owner = r.ownerFor(base)
	activeBrowserRunner = &selectingRunner{host: markerRunner{"host", new(int)}, container: r}
	adoptionSupported = true
	return m, reg, func() {
		activeBrowserRunner, adoptionSupported = prev, prevAdopt
		detachedWorker.Store(prevDet)
		registryDirResolver = regDirResolverPrev
	}
}

func writeContainerRecord(t *testing.T, m *CobrowseSessionManager, reg string, mod func(*sessionRecord)) sessionRecord {
	dir := filepath.Join(m.baseDir, testSID)
	os.MkdirAll(dir, 0o700)
	uid := os.Getuid()
	rec := goodRecord()
	rec.Profile = dir
	rec.State = RecordLive
	rec.OwnerPID = os.Getpid()
	rec.OwnerStart = currentOwnerIdentity()
	rec.OwnerUID = &uid
	if mod != nil {
		mod(&rec)
	}
	if err := saveRecord(reg, rec); err != nil {
		t.Skipf("registry unavailable here: %v", err)
	}
	return rec
}

func TestRecoverContainerAdoptsVerifiedRecord(t *testing.T) {
	f := &fakeEngine{}
	m, reg, restore := containerManager(t, f)
	defer restore()
	if currentOwnerIdentity() == 0 {
		t.Skip("needs /proc")
	}
	writeContainerRecord(t, m, reg, nil)
	m.recoverSessions()
	m.mu.Lock()
	s := m.sessions[testSID]
	m.mu.Unlock()
	if s == nil || s.proc == nil || s.proc.containerID != testCID {
		t.Fatal("verified container record must be re-adopted")
	}
	if s.workDir != filepath.Join(m.baseDir, testSID, containerProfileSubdir) {
		t.Fatal("adopted session must keep its profile subdir")
	}
	s.proc.stop()
}

func TestRecoverContainerExpiresNotRunning(t *testing.T) {
	f := &fakeEngine{notRunning: true}
	m, reg, restore := containerManager(t, f)
	defer restore()
	if currentOwnerIdentity() == 0 {
		t.Skip("needs /proc")
	}
	writeContainerRecord(t, m, reg, nil)
	m.recoverSessions()
	rec, err := loadRecord(reg, testSID)
	if err != nil || rec.State != RecordExpired || rec.EndedReason == "" {
		t.Fatalf("a record that fails verification must be expired with a reason: %+v %v", rec, err)
	}
	if len(m.sessions) != 0 {
		t.Fatal("must not adopt")
	}
	if f.has("rm", "-f", testCID) != 1 {
		t.Fatalf("the labelled container must be discarded: %v", f.calls)
	}
}

func TestRecoverContainerLeavesTransientAndForeignRecords(t *testing.T) {
	// CDP identity probe that errors: transient, record stays live and untouched.
	f := &fakeEngine{}
	m, reg, restore := containerManager(t, f)
	defer restore()
	if currentOwnerIdentity() == 0 {
		t.Skip("needs /proc")
	}
	sr := activeBrowserRunner.(*selectingRunner)
	sr.container.browserIDAt = func(context.Context, int) (string, error) { return "", errors.New("slow") }
	writeContainerRecord(t, m, reg, nil)
	m.recoverSessions()
	if rec, _ := loadRecord(reg, testSID); rec.State != RecordLive {
		t.Fatalf("errCDPCheck must leave the record live, got %q", rec.State)
	}
	if f.has("rm", "-f") != 0 {
		t.Fatal("must not remove the container on a transient error")
	}

	// A record from a dead owner seen by a non-managed process: not ours to expire.
	f2 := &fakeEngine{}
	m2, reg2, restore2 := containerManager(t, f2)
	defer restore2()
	detachedWorker.Store(false)
	writeContainerRecord(t, m2, reg2, func(r *sessionRecord) { r.OwnerPID = 999999; r.OwnerStart = 12345 })
	m2.recoverSessions()
	if rec, _ := loadRecord(reg2, testSID); rec.State != RecordLive {
		t.Fatalf("a non-managed process must not expire the record, got %q", rec.State)
	}
	if f2.has("rm", "-f") != 0 {
		t.Fatal("and must not remove its container")
	}
}

func TestVerifyContainerRecordStructure(t *testing.T) {
	f := &fakeEngine{}
	m, _, restore := containerManager(t, f)
	defer restore()
	ad := activeBrowserRunner.(containerAdopter)
	dir := filepath.Join(m.baseDir, testSID)
	os.MkdirAll(dir, 0o700)
	good := goodRecord()
	good.Profile = dir
	good.OwnerPID, good.OwnerStart = os.Getpid(), currentOwnerIdentity()
	if currentOwnerIdentity() == 0 {
		t.Skip("needs /proc")
	}
	if err := m.verifyContainerRecord(good, dir, ad); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*sessionRecord){
		"profile mismatch": func(r *sessionRecord) { r.Profile = "/elsewhere" },
		"persistent":       func(r *sessionRecord) { r.Persistent = true },
		"version":          func(r *sessionRecord) { r.Version = 1 },
	} {
		r := good
		mod(&r)
		if m.verifyContainerRecord(r, dir, ad) == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	if m.verifyContainerRecord(good, dir, nil) == nil {
		t.Error("no runtime must refuse")
	}
}

func TestShutdownRetainsContainerSessions(t *testing.T) {
	f := &fakeEngine{}
	m, _, restore := containerManager(t, f)
	defer restore()
	detachedWorker.Store(true)
	stops := 0
	s := &cobrowseSession{id: testSID, profile: filepath.Join(m.baseDir, testSID), state: SessionRunning,
		proc: &cobrowseProc{containerID: testCID, stop: func() error { stops++; return nil }}}
	m.sessions[testSID] = s
	_ = m.ShutdownSessions()
	if stops != 0 {
		t.Fatal("a managed worker shutdown must leave container sessions running for re-adoption")
	}
	// A persistent-profile container session is still sealed and stopped.
	s2 := &cobrowseSession{id: "cb-1111111111111111", profile: filepath.Join(m.baseDir, "x"), state: SessionRunning, persistent: true,
		proc: &cobrowseProc{containerID: testCID, stop: func() error { stops++; return nil }}}
	m.sessions[s2.id] = s2
	_ = m.ShutdownSessions()
	if stops != 1 {
		t.Fatal("persistent sessions must still be stopped")
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
	p := &cobrowseProc{containerID: testCID, containerStage: "/h/cb-1/.citadel-stage"}
	if got, err := p.browserFilePath("/h/cb-1/.citadel-stage/a.txt"); err != nil || got != browserruntime.StageMountPath+"/a.txt" {
		t.Fatal(got, err)
	}
	for _, bad := range []string{"/elsewhere/a", "/h/cb-1/.citadel-stage", "/h/cb-1/.citadel-stage/../x", "/h/cb-1/profile/Cookies"} {
		if _, err := p.browserFilePath(bad); err == nil {
			t.Fatalf("container proc must fail closed for %q", bad)
		}
	}
	if got, err := (&cobrowseProc{}).browserFilePath("/h/a"); err != nil || got != "/h/a" {
		t.Fatal("host procs keep the host path")
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

func TestDiscardRequiresOwnerPurposeAndNetworkLabels(t *testing.T) {
	for name, f := range map[string]*fakeEngine{
		"foreign owner":   {owner: "someoneelse", foreignNet: true},
		"foreign purpose": {foreignPurpose: true, foreignNet: true},
	} {
		t.Run(name, func(t *testing.T) {
			r := newTestRunner(f, false)
			if f.owner == "" {
				f.owner = r.ownerFor("/x")
			}
			r.discardContainer(goodRecord(), "/x/"+testSID)
			if f.has("rm") != 0 || f.has("network", "rm") != 0 {
				t.Fatalf("must not remove another owner's or purpose's objects: %v", f.calls)
			}
		})
	}
	// Our container but a foreign-labelled network: only the container goes.
	f := &fakeEngine{foreignNet: true}
	r := adoptRunner(f)
	r.discardContainer(goodRecord(), "/x/"+testSID)
	if f.has("rm", "-f", testCID) != 1 || f.has("network", "rm") != 0 {
		t.Fatalf("network removal needs its own labels: %v", f.calls)
	}
}

func TestRecoverContainerOwnedByAnotherWorkerIsUntouched(t *testing.T) {
	for name, mod := range map[string]func(*fakeEngine){
		"owner":   func(f *fakeEngine) { f.owner = "someoneelse"; f.foreignNet = true },
		"purpose": func(f *fakeEngine) { f.foreignPurpose = true; f.foreignNet = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeEngine{}
			m, reg, restore := containerManager(t, f)
			defer restore()
			if currentOwnerIdentity() == 0 {
				t.Skip("needs /proc")
			}
			mod(f)
			writeContainerRecord(t, m, reg, nil)
			m.recoverSessions()
			if f.has("rm") != 0 || f.has("network", "rm") != 0 {
				t.Fatalf("recovery removed another worker's container or network: %v", f.calls)
			}
			if rec, _ := loadRecord(reg, testSID); rec.State != RecordLive {
				t.Fatalf("record must be left as is, got %q", rec.State)
			}
			if len(m.sessions) != 0 {
				t.Fatal("must not adopt it")
			}
		})
	}
}

func TestDefaultStartBrowserRequiresSandboxedChromium(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		ok   bool
	}{
		"confirmed":   {`{"browser":"chromium","sandboxed":true}`, true},
		"unsandboxed": {`{"browser":"chromium","sandboxed":false}`, false},
		"other":       {`{"browser":"cloakbrowser","sandboxed":true}`, false},
		"empty":       {``, false},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			port, _ := strconv.Atoi(srv.URL[strings.LastIndex(srv.URL, ":")+1:])
			err := defaultStartBrowser(context.Background(), port, "s1")
			if (err == nil) != tc.ok {
				t.Fatalf("err=%v want ok=%v", err, tc.ok)
			}
		})
	}
}
