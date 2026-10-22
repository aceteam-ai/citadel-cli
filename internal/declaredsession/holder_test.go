package declaredsession

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/procscope"
)

// All process/terminal/scope operations here are injected fakes. These fixtures
// do not start a harness, open a PTY/socket or contact a service manager.
type fakeRecords struct {
	order  []string
	states []State
	marker *StopMarker
	fail   bool
}

func (f *fakeRecords) WriteState(state State) error {
	f.order = append(f.order, "state:"+state.Phase)
	f.states = append(f.states, state)
	return nil
}

func (f *fakeRecords) WriteStopMarker(marker StopMarker) error {
	f.order = append(f.order, "marker")
	if f.fail {
		return ErrUnavailable
	}
	f.marker = &marker
	return nil
}

type fakeConfiguration struct{ snapshot Snapshot }

func (f fakeConfiguration) LoadConfig() (Snapshot, error) { return f.snapshot, nil }

type fakeHolderLogs struct {
	console []byte
	events  []Event
}

func (f *fakeHolderLogs) AppendConsole(data []byte) error {
	f.console = append(f.console, data...)
	return nil
}

func (f *fakeHolderLogs) AppendEvent(event Event) error {
	f.events = append(f.events, event)
	return nil
}

type fakeScope struct {
	mu      sync.Mutex
	allowed [][]procscope.ProcessIdentity
	fail    bool
}

func (f *fakeScope) CheckMembers(_ context.Context, allowed []procscope.ProcessIdentity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allowed = append(f.allowed, append([]procscope.ProcessIdentity(nil), allowed...))
	if f.fail {
		return ErrUnavailable
	}
	return nil
}

type fakeHolderChild struct {
	identity ProcessIdentity
	exited   chan struct{}
	mu       sync.Mutex
	calls    []string
	writes   [][]byte
	onWrite  func(context.Context, []byte) (int, error)
	onSignal func(int) error
	exit     Exit
}

func newFakeChild() *fakeHolderChild {
	exited := make(chan struct{})
	close(exited)
	return &fakeHolderChild{identity: ProcessIdentity{PID: 77, StartTicks: 8, UID: 1000, BootID: "boot"}, exited: exited}
}

func (f *fakeHolderChild) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeHolderChild) Identity() ProcessIdentity { return f.identity }
func (f *fakeHolderChild) Exited() <-chan struct{}   { return f.exited }
func (f *fakeHolderChild) Read(context.Context, []byte) (int, error) {
	return 0, io.EOF
}
func (f *fakeHolderChild) Write(ctx context.Context, data []byte) (int, error) {
	if f.onWrite != nil {
		return f.onWrite(ctx, data)
	}
	f.mu.Lock()
	f.writes = append(f.writes, append([]byte(nil), data...))
	f.mu.Unlock()
	return len(data), nil
}
func (f *fakeHolderChild) Resize(uint16, uint16) error {
	f.record("resize")
	return nil
}
func (f *fakeHolderChild) Repaint() error {
	f.record("repaint")
	return nil
}
func (f *fakeHolderChild) SignalGroup(signal int) error {
	f.record("signal")
	if f.onSignal != nil {
		return f.onSignal(signal)
	}
	return nil
}
func (f *fakeHolderChild) Reap() (Exit, error) {
	f.record("reap")
	return f.exit, nil
}
func (f *fakeHolderChild) Close() error {
	f.record("close")
	return nil
}

func holderFixture() (*holder, *fakeRecords) {
	d := defaultDeclaration()
	d.Name, d.Revision = "test", "sha256:"+strings.Repeat("a", 64)
	d.Command, d.Cwd = []string{"harness", "original"}, "/home/test"
	d.Harness = "other"
	state := State{Version: 1, Name: "test", Incarnation: strings.Repeat("b", 32), Holder: ProcessIdentity{PID: 55, StartTicks: 3, UID: 1000, BootID: "boot"}, DeclarationRevision: d.Revision, Phase: "running", ChildGeneration: 1}
	h := newHolder(context.Background(), d, state)
	records := &fakeRecords{}
	h.session, h.logs, h.scope = records, &fakeHolderLogs{}, &fakeScope{}
	h.store = fakeConfiguration{snapshot: Snapshot{Sessions: []Declaration{d}}}
	h.environment = SpawnSpec{Env: []string{"PATH=/bin"}}
	h.resolve = func(command, _ string, _ []string) (string, error) { return "/bin/" + command, nil }
	return h, records
}

func testViewer(id string, readonly bool) *holderViewer {
	return &holderViewer{id: id, ceiling: readonly, queue: make(chan wireFrame, viewerQueueSize), cancel: func() {}}
}

func TestReplayJoinHasNoGapOrDuplicateAndTrimsWholeChunks(t *testing.T) {
	h, _ := holderFixture()
	h.ring.limit = 5
	h.publishOutput([]byte("one"))
	viewer := testViewer("reader", true)
	request := frame("attach")
	request.Incarnation, request.ReadOnly, request.Cols, request.Rows = h.state.Incarnation, true, 80, 24
	reply := h.attach(holderRequest{viewer: viewer, frame: request})
	if reply.err != nil || len(reply.replay) != 1 || reply.replay[0].sequence != 1 {
		t.Fatalf("backlog: %#v", reply)
	}
	h.publishOutput([]byte("two"))
	if len(h.ring.chunks) != 1 || string(h.ring.chunks[0].data) != "two" {
		t.Fatal("ring did not evict the entire old chunk")
	}
	<-viewer.queue // dimensions precede live output, not a second backlog copy
	live := <-viewer.queue
	if live.Op != "output" || live.Sequence != 2 || string(live.Data) != "two" {
		t.Fatalf("live sequence mismatch: %#v", live)
	}
	h.ring.append(3, []byte("too-long"))
	if len(h.ring.chunks) != 1 || string(h.ring.chunks[0].data) != "two" {
		t.Fatal("oversized chunk must not become a truncated replay tail")
	}
}

func TestSlowViewerIsDisconnectedWithoutClosingChildOrOtherViewer(t *testing.T) {
	h, _ := holderFixture()
	child := newFakeChild()
	h.child = child
	slow, fast := testViewer("slow", true), testViewer("fast", true)
	detached := false
	slow.cancel = func() { detached = true }
	for i := 0; i < cap(slow.queue); i++ {
		slow.queue <- frame("pong")
	}
	h.viewers[slow.id], h.viewers[fast.id] = slow, fast
	h.publishOutput([]byte("data"))
	if !detached || h.viewers[slow.id] != nil || h.viewers[fast.id] == nil || len(child.calls) != 0 {
		t.Fatal("slow viewer affected harness or another viewer")
	}
	if got := <-fast.queue; string(got.Data) != "data" {
		t.Fatal("fast viewer lost output")
	}
	h.removeViewer(fast.id, "viewer_detached")
	if len(child.calls) != 0 {
		t.Fatal("detach killed or closed harness")
	}
}

func TestReadonlyCeilingIsEnforcedAtHolderNotClient(t *testing.T) {
	h, _ := holderFixture()
	viewer := testViewer("reader", true)
	h.viewers[viewer.id] = viewer
	h.controller = viewer.id // even corrupted role bookkeeping cannot bypass ceiling
	for _, op := range []string{"input", "resize", "take_control"} {
		f := frame(op)
		f.Data, f.Cols, f.Rows = []byte("secret"), 90, 30
		if reply := h.viewerOperation(holderRequest{viewer: viewer, frame: f}); !errors.Is(reply.err, ErrRole) {
			t.Fatalf("readonly %s granted", op)
		}
	}
}

func TestControllerTransferWaitsBlockedWriteAndDropsQueuedOldFrames(t *testing.T) {
	h, _ := holderFixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.ctx = ctx
	child := newFakeChild()
	entered, release := make(chan struct{}), make(chan struct{})
	child.onWrite = func(ctx context.Context, data []byte) (int, error) {
		close(entered)
		select {
		case <-release:
			return len(data), nil
		case <-ctx.Done():
			return 0, errChildIO
		}
	}
	h.terminal = newTerminalIO(ctx, child, 1)
	defer h.terminal.stop()
	h.epoch = 1
	h.terminal.epoch.Store(1)
	old, next := testViewer("old", false), testViewer("next", false)
	next.cols, next.rows = 100, 40
	h.viewers[old.id], h.viewers[next.id], h.controller = old, next, old.id
	h.terminal.enqueue(inputOperation{epoch: 1, data: []byte("inflight")})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("fake write not reached")
	}
	h.terminal.enqueue(inputOperation{epoch: 1, data: []byte("must-drop")})
	if !h.transfer(next.id) {
		t.Fatal("transfer refused")
	}
	h.publishOutput([]byte("fanout-is-not-blocked"))
	if h.controller != "" {
		t.Fatal("new controller granted before old write ended")
	}
	select {
	case <-h.controls:
		t.Fatal("barrier passed blocked write")
	default:
	}
	close(release)
	var result controlReady
	select {
	case result = <-h.controls:
	case <-time.After(time.Second):
		t.Fatal("transfer barrier missing")
	}
	h.acceptControl(result)
	if h.controller != next.id {
		t.Fatal("controller not granted after barrier")
	}
	// If stale queued data reached Write, fake's second close(entered) panics.
}

func TestCleanupRetainsLeaderThroughProofThenReapsOnce(t *testing.T) {
	h, _ := holderFixture()
	child := newFakeChild()
	h.child = child
	h.readDone = make(chan struct{})
	close(h.readDone)
	h.readEOF = true
	h.beginCleanup(true)
	var result cleanupResult
	select {
	case result = <-h.cleaned:
	case <-time.After(time.Second):
		t.Fatal("cleanup blocked")
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	if !reflect.DeepEqual(child.calls, []string{"signal", "reap", "close"}) {
		t.Fatal(child.calls)
	}
	scope := h.scope.(*fakeScope)
	if len(scope.allowed) != 2 || len(scope.allowed[0]) != 2 || len(scope.allowed[1]) != 1 || scope.allowed[0][1].PID != child.identity.PID {
		t.Fatal("cleanup did not prove retained leader then holder-only scope")
	}
	if !h.finishCleanup(result) {
		t.Fatal("stop did not finish")
	}
}

func TestExitedLeaderStillEscalatesTermResistantGroupBeforeReap(t *testing.T) {
	h, _ := holderFixture()
	child := newFakeChild() // leader.Exited already closed, but slave still held
	h.child = child
	h.readDone = make(chan struct{})
	var signals []int
	child.onSignal = func(signal int) error {
		signals = append(signals, signal)
		if signal == 9 {
			close(h.readDone)
		}
		return nil
	}
	h.await = func(_ context.Context, done <-chan struct{}, _ time.Duration) error {
		// Inject the grace-expired observation; no timing/sleep/native process.
		select {
		case <-done:
			return nil
		default:
			return errChildIO
		}
	}
	h.beginCleanup(true)
	select {
	case result := <-h.cleaned:
		if result.err != nil || !reflect.DeepEqual(signals, []int{15, 9}) || !reflect.DeepEqual(child.calls, []string{"signal", "signal", "reap", "close"}) {
			t.Fatal("leader exit skipped group escalation", signals, child.calls)
		}
	case <-time.After(time.Second):
		t.Fatal("injected cleanup blocked")
	}
}

func TestCleanupPublishesReadonlyRevocationWithoutUpgradingReadonlyCeiling(t *testing.T) {
	h, _ := holderFixture()
	controller, readonly := testViewer("controller", false), testViewer("readonly", true)
	h.controller = controller.id
	h.viewers[controller.id], h.viewers[readonly.id] = controller, readonly
	h.beginCleanup(true) // no child; fake scope proves holder-only
	for _, viewer := range []*holderViewer{controller, readonly} {
		select {
		case role := <-viewer.queue:
			if role.Op != "role" || !role.ReadOnly {
				t.Fatal("cleanup left client believing it controlled input")
			}
		default:
			t.Fatal("missing role revocation")
		}
	}
	if h.controller != "" || !readonly.ceiling || controller.ceiling {
		t.Fatal("revocation changed immutable connection eligibility")
	}
	select {
	case <-h.cleaned:
	case <-time.After(time.Second):
		t.Fatal("cleanup fixture blocked")
	}
}

func TestExpiredQueuedInputNeverReachesTerminal(t *testing.T) {
	child := newFakeChild()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	terminal := newTerminalIO(ctx, child, 1)
	defer terminal.stop()
	terminal.epoch.Store(1)
	terminal.queue <- inputOperation{epoch: 1, data: []byte("expired"), until: time.Now().Add(-time.Second)}
	barrier := make(chan struct{})
	terminal.queue <- inputOperation{barrier: barrier}
	select {
	case <-barrier:
	case <-time.After(time.Second):
		t.Fatal("queue did not drain")
	}
	child.mu.Lock()
	defer child.mu.Unlock()
	if len(child.writes) != 0 {
		t.Fatal("expired queued input executed")
	}
}

func TestEscapedScopeMemberQuarantinesWithoutReapOrRespawn(t *testing.T) {
	h, _ := holderFixture()
	child := newFakeChild()
	h.child = child
	h.readDone = make(chan struct{})
	close(h.readDone)
	h.scope.(*fakeScope).fail = true
	h.beginCleanup(false)
	var result cleanupResult
	select {
	case result = <-h.cleaned:
	case <-time.After(time.Second):
		t.Fatal("cleanup blocked")
	}
	if result.err == nil || h.finishCleanup(result) || !h.quarantined || h.state.Phase != "cleanup_unconfirmed" {
		t.Fatal("uncertain cleanup released custody")
	}
	if !reflect.DeepEqual(child.calls, []string{"signal"}) {
		t.Fatal("leader reaped before proof", child.calls)
	}
	called := false
	h.spawn = func(Declaration, []string, []string, uint64) (holderChild, error) { called = true; return nil, nil }
	if h.startChild() == nil || called {
		t.Fatal("quarantined holder respawned")
	}
}

func TestCleanExitMarkerPrecedesStoppedStateAndRevisionConflictStillVetoes(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		h, records := holderFixture()
		h.readEOF = true
		if conflict {
			h.store = fakeConfiguration{snapshot: Snapshot{}}
		}
		if !h.finishCleanup(cleanupResult{exit: Exit{Code: 0}}) {
			t.Fatal("clean exit did not stop")
		}
		want := "stopped_clean"
		if conflict {
			want = "config_conflict"
		}
		if records.marker == nil || records.marker.DeclarationRevision != h.declaration.Revision || !reflect.DeepEqual(records.order, []string{"marker", "state:" + want}) {
			t.Fatalf("marker-first/conflict contract: %#v", records)
		}
	}
	h, records := holderFixture()
	h.readEOF, records.fail = true, true
	h.finishCleanup(cleanupResult{exit: Exit{Code: 0}})
	if h.state.Phase != "error" || !h.storageFailed || records.marker != nil {
		t.Fatal("failed marker claimed durable clean stop")
	}
}

func TestAutomaticRestartPreservesViewersAndUsesOriginalArgvWithoutResume(t *testing.T) {
	h, _ := holderFixture()
	h.readEOF = true
	h.spawnAt = time.Now()
	viewer := testViewer("viewer", true)
	h.viewers[viewer.id] = viewer
	h.publishOutput([]byte("old"))
	if h.finishCleanup(cleanupResult{exit: Exit{Code: 1}}) || h.nextSpawn.IsZero() {
		t.Fatal("failed child did not schedule restart")
	}
	var argv []string
	h.spawn = func(_ Declaration, args, _ []string, generation uint64) (holderChild, error) {
		argv = append([]string(nil), args...)
		if generation != 2 {
			t.Fatal("generation did not advance")
		}
		return newFakeChild(), nil
	}
	if err := h.startChild(); err != nil {
		t.Fatal(err)
	}
	defer h.terminal.stop()
	defer func() {
		h.readCancel()
		<-h.readDone
	}()
	if !reflect.DeepEqual(argv, []string{"/bin/harness", "original"}) || h.viewers[viewer.id] != viewer || h.sequence != 1 {
		t.Fatal("restart replaced viewer/ring or invented harness args")
	}
}

func TestRestartArgvChoiceIsExplicitAndNeverInventsResumeFlags(t *testing.T) {
	for _, test := range []struct {
		name       string
		generation uint64
		explicit   bool
		override   []string
		want       []string
	}{
		{"initial", 0, false, []string{"resume", "declared"}, []string{"/bin/harness", "original"}},
		{"automatic", 1, false, []string{"resume", "declared"}, []string{"/bin/resume", "declared"}},
		{"explicit", 0, true, []string{"resume", "declared"}, []string{"/bin/resume", "declared"}},
		{"explicit-no-override", 0, true, nil, []string{"/bin/harness", "original"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, _ := holderFixture()
			h.state.ChildGeneration, h.restartCommand = test.generation, test.explicit
			h.declaration.CommandRestart = test.override
			var got []string
			h.spawn = func(_ Declaration, args, _ []string, _ uint64) (holderChild, error) {
				got = append([]string(nil), args...)
				return newFakeChild(), nil
			}
			if h.startChild() != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("argv: %q", got)
			}
			h.terminal.stop()
			h.readCancel()
			<-h.readDone
		})
	}
}

func TestExplicitRestartPreparationDoesNotRequireOriginalExecutable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("executable-mode fixture is Linux-only; never executes the file")
	}
	dir := t.TempDir()
	resume := filepath.Join(dir, "resume")
	if os.WriteFile(resume, []byte("fixture, never executed"), 0700) != nil {
		t.Fatal("fixture file unavailable")
	}
	d := defaultDeclaration()
	d.Name, d.Harness, d.Cwd = "test", "other", dir
	d.Command = []string{"nonexistent-original"}
	d.CommandRestart = []string{"resume", "declared"}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	d.Revision = digest(raw)
	store := &Store{uid: 1000} // pure BuildSpawn inputs; no storage operations
	owner := OwnerContext{UID: 1000, Name: "test", Home: dir, Hostname: "node"}
	if _, err := store.BuildSpawn(d, false, owner, map[string]string{"PATH": dir}); err == nil {
		t.Fatal("missing original fixture unexpectedly resolved")
	}
	spawn, err := store.BuildSpawn(d, true, owner, map[string]string{"PATH": dir})
	if err != nil || !reflect.DeepEqual(spawn.Argv, []string{resume, "declared"}) {
		t.Fatal("explicit restart resolved original instead of declared restart argv")
	}
}

func TestEssentialEnvironmentNeverQueriesOrInheritsAmbientDeviceKeys(t *testing.T) {
	queried := map[string]bool{}
	got := essentialEnvironment(func(key string) (string, bool) {
		queried[key] = true
		return "value", true
	})
	for _, secret := range []string{"ACETEAM_API_KEY", "CITADEL_DEVICE_API_TOKEN", "INTERNAL_AUTH_SECRET", "TS_AUTHKEY", "UNDECLARED_ENV"} {
		if queried[secret] || got[secret] != "" {
			t.Fatal("ambient secret inherited")
		}
	}
	if got["PATH"] != "value" || got["TERM"] != "value" {
		t.Fatal("essential launcher values lost")
	}
}

func TestReplayChunkCountBoundsTinyReadMetadata(t *testing.T) {
	ring := replayRing{limit: 4 << 20}
	for i := 0; i < maxReplayChunks+10; i++ {
		ring.append(uint64(i+1), []byte("x"))
	}
	if len(ring.chunks) != maxReplayChunks || ring.bytes != maxReplayChunks || ring.chunks[0].sequence != 11 {
		t.Fatal("tiny output reads grew replay metadata without a bound")
	}
}

func TestRestartJitterIsBoundedAndNeverReducesMinimum(t *testing.T) {
	for _, random := range []byte{0, 1, 128, 255} {
		got := restartDelay(time.Second, 2*time.Second, random)
		if got < time.Second || got > 1250*time.Millisecond {
			t.Fatal(got)
		}
		if restartDelay(2*time.Second, 2*time.Second, random) != 2*time.Second {
			t.Fatal("jitter exceeded configured maximum")
		}
	}
}

func TestPendingLaunchBindsReservedDeclarationAndExactLauncher(t *testing.T) {
	owner := ProcessIdentity{PID: 42, UID: 1000, StartTicks: 123, BootID: "boot"}
	opts := RunHolderOptions{Name: "test", ExpectedUnit: "citadel-sess-test.scope", ExpectedIncarnation: strings.Repeat("a", 32), BinaryVersion: "fixture-version"}
	declaration := Declaration{Name: "test", Revision: digest([]byte("reserved command"))}
	pending := State{Version: 1, Name: opts.Name, Incarnation: opts.ExpectedIncarnation, DeclarationRevision: declaration.Revision, ProtocolVersion: ProtocolVersion, BinaryVersion: opts.BinaryVersion, Phase: "starting", Guarantee: "unconfirmed", Scope: ScopeIdentity{Name: opts.ExpectedUnit, Manager: "user"}}
	if !pendingLaunchMatches(pending, opts, declaration, owner) {
		t.Fatal("reserved launch without a captured birth was refused")
	}
	pending.Holder = owner
	pending.FileRevision = digest([]byte("unrelated session changed"))
	if !pendingLaunchMatches(pending, opts, declaration, owner) {
		t.Fatal("matching captured launcher or unrelated file edit was refused")
	}
	for _, mutate := range []func(*State){
		func(s *State) { s.Version++ },
		func(s *State) { s.Name = "other" },
		func(s *State) { s.Incarnation = strings.Repeat("b", 32) },
		func(s *State) { s.DeclarationRevision = digest([]byte("substituted command")) },
		func(s *State) { s.ProtocolVersion++ },
		func(s *State) { s.BinaryVersion = "other" },
		func(s *State) { s.Phase = "running" },
		func(s *State) { s.Guarantee = "verified_user_scope" },
		func(s *State) { s.Scope.InvocationID = strings.Repeat("c", 32) },
		func(s *State) { s.Scope.Name = "citadel-sess-other.scope" },
		func(s *State) { s.Holder.StartTicks++ },
		func(s *State) { s.Holder.PID++ },
		func(s *State) { s.Holder = ProcessIdentity{UID: owner.UID} },
	} {
		changed := pending
		mutate(&changed)
		if pendingLaunchMatches(changed, opts, declaration, owner) {
			t.Fatal("changed pending execution authority was accepted")
		}
	}
	declaration.Revision = digest([]byte("human edit during launch"))
	if pendingLaunchMatches(pending, opts, declaration, owner) {
		t.Fatal("newly loaded command replaced the reserved declaration")
	}
}
