package declaredsession

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/procscope"
)

type RunHolderOptions struct {
	NodeDir             string
	Name                string
	ExpectedUnit        string
	ExpectedIncarnation string
	BinaryVersion       string
	OriginControlGroup  string
	Foreground          bool
	RestartCommand      bool
}

type scopeCustody interface {
	CheckMembers(context.Context, []procscope.ProcessIdentity) error
}

type holderRecords interface {
	WriteState(State) error
	WriteStopMarker(StopMarker) error
}

type holderConfiguration interface {
	LoadConfig() (Snapshot, error)
}

type holderLogs interface {
	AppendConsole([]byte) error
	AppendEvent(Event) error
}

type cleanupResult struct {
	exit    Exit
	err     error
	noChild bool
}

const (
	maxConsoleBatchBytes = 256 << 10
	maxPendingEvents     = 128
	consoleFlushInterval = 50 * time.Millisecond
	holderRecordWait     = 50 * time.Millisecond
)

type holder struct {
	ctx            context.Context
	declaration    Declaration
	store          holderConfiguration
	session        holderRecords
	logs           holderLogs
	scope          scopeCustody
	state          State
	environment    SpawnSpec
	requests       chan holderRequest
	parts          chan outputPart
	controls       chan controlReady
	cleaned        chan cleanupResult
	viewers        map[string]*holderViewer
	ring           replayRing
	sequence       uint64
	controller     string
	epoch          uint64
	controlPending bool
	cols           uint16
	rows           uint16
	child          holderChild
	terminal       *terminalIO
	readCancel     context.CancelFunc
	readDone       chan struct{}
	readEOF        bool
	cleaning       bool
	quarantined    bool
	stopRequested  bool
	stopReason     string
	storageFailed  bool
	consolePending []byte
	eventsPending  []Event
	markerPending  *StopMarker
	statePending   bool
	outputPending  *outputPart
	cleanupPending *cleanupResult
	exitAfterFlush bool
	stopReplies    []holderRequest
	spawnAt        time.Time
	nextSpawn      time.Time
	backoff        time.Duration
	rapidExits     int
	restartCommand bool
	spawn          func(Declaration, []string, []string, uint64) (holderChild, error)
	resolve        func(string, string, []string) (string, error)
	await          func(context.Context, <-chan struct{}, time.Duration) error
}

// RunHolder is the production entry. Scope verification cannot be skipped by
// constructing a DTO or passing a nil proof. Foreground custody is unsupported
// until a distinct containment contract exists; it never means verified scope.
func RunHolder(ctx context.Context, options RunHolderOptions) (result error) {
	if options.Foreground || ValidateName(options.Name) != nil || !filepath.IsAbs(options.NodeDir) || options.ExpectedUnit != "citadel-sess-"+options.Name+".scope" || !validIncarnation(options.ExpectedIncarnation) || len(options.BinaryVersion) > 128 {
		return ErrConfig
	}
	owner, err := nativeOwner()
	if err != nil {
		return err
	}
	scope, err := procscope.VerifySelf(ctx, procscope.ScopeSpec{Unit: options.ExpectedUnit, UID: owner.UID, OriginControlGroup: options.OriginControlGroup})
	if err != nil {
		return ErrUnavailable
	}
	defer func() {
		if scope.Close() != nil {
			result = ErrUnavailable
		}
	}()
	if scope.CheckMembers(ctx, []procscope.ProcessIdentity{toScopeProcess(owner)}) != nil {
		return ErrUnavailable
	}
	store, err := OpenStore(options.NodeDir)
	if err != nil {
		return err
	}
	defer func() {
		if store.Close() != nil {
			result = ErrUnavailable
		}
	}()
	snapshot, err := store.LoadConfig()
	declaration, exists := snapshot.Lookup(options.Name)
	if err != nil || !exists || !declaration.Enabled || declaration.Mode != "interactive" {
		return ErrConfig
	}
	session, err := store.OpenSession(options.Name, true)
	if err != nil {
		return err
	}
	// This handle belongs only to the holder and is not published yet. Keep
	// actor record-lock contention to one flush interval, retaining exact
	// pending records on ErrBusy. This does not bound filesystem or fsync I/O.
	session.recordWait = holderRecordWait
	defer func() {
		if session.Close() != nil {
			result = ErrUnavailable
		}
	}()
	guard, err := session.AcquireOwner(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if guard.Close() != nil {
			result = ErrUnavailable
		}
	}() // release after socket/process/log cleanup
	if _, present, err := session.ReadStopMarker(); err != nil || present {
		return ErrConflict
	}
	pending, present, err := session.ReadState()
	if err != nil || !present || !pendingLaunchMatches(pending, options, declaration, owner) {
		return ErrConflict
	}
	ownerContext, essential, err := holderEnvironment(owner.UID)
	if err != nil {
		return err
	}
	environment, err := store.BuildSpawn(declaration, options.RestartCommand, ownerContext, essential)
	if err != nil {
		return err
	}
	logs, err := session.OpenLogs(declaration.Log)
	if err != nil {
		return err
	}
	identity := scope.Identity()
	now := time.Now().UTC()
	state := State{Version: 1, Name: declaration.Name, Incarnation: options.ExpectedIncarnation, Holder: owner, Scope: ScopeIdentity{Name: identity.Name, Manager: "user", InvocationID: identity.InvocationID, ControlGroup: identity.ControlGroup}, ProtocolVersion: ProtocolVersion, BinaryVersion: options.BinaryVersion, StartedAt: now, UpdatedAt: now, FileRevision: snapshot.FileRevision, DeclarationRevision: declaration.Revision, Phase: "starting", Guarantee: "verified_user_scope", EnvironmentHash: environment.EnvironmentHash}
	if session.WriteState(state) != nil {
		return ErrUnavailable
	}
	listener, err := listenHolder(session)
	if err != nil {
		return err
	}
	defer func() {
		if listener.Close() != nil {
			result = ErrUnavailable
		}
	}()
	holderCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHolder(holderCtx, declaration, state)
	h.store, h.session, h.logs, h.scope, h.environment = store, session, logs, scope, environment
	h.restartCommand = options.RestartCommand
	h.spawn = spawnTerminal
	servers := newHolderServers(holderCtx, h, listener)
	defer servers.close()
	return h.run(ctx)
}

// Bind the managed launch to the declaration which the operator actually
// reserved. A human edit after manager recheck must not substitute a different
// command before the holder starts. Unrelated declarations may change the full
// file revision; only this normalized declaration is execution authority.
func pendingLaunchMatches(pending State, options RunHolderOptions, declaration Declaration, owner ProcessIdentity) bool {
	return pending.Version == 1 && pending.Name == options.Name && declaration.Name == options.Name &&
		pending.Incarnation == options.ExpectedIncarnation && pending.DeclarationRevision == declaration.Revision &&
		pending.ProtocolVersion == ProtocolVersion && pending.BinaryVersion == options.BinaryVersion &&
		pending.Phase == "starting" && pending.Guarantee == "unconfirmed" &&
		pending.Scope == (ScopeIdentity{Name: options.ExpectedUnit, Manager: "user"}) &&
		(pending.Holder == (ProcessIdentity{}) || pending.Holder == owner)
}

func newHolder(ctx context.Context, declaration Declaration, state State) *holder {
	return &holder{ctx: ctx, declaration: declaration, state: state, requests: make(chan holderRequest, 32), parts: make(chan outputPart, 32), controls: make(chan controlReady, 1), cleaned: make(chan cleanupResult, 1), viewers: map[string]*holderViewer{}, ring: replayRing{limit: declaration.ScrollbackBytes}, cols: 80, rows: 24, resolve: resolveExecutable, await: waitClosed}
}

func holderEnvironment(uid uint32) (OwnerContext, map[string]string, error) {
	owner, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return OwnerContext{}, nil, ErrConfig
	}
	hostname, err := os.Hostname()
	if err != nil {
		return OwnerContext{}, nil, ErrConfig
	}
	// No os.Environ append. Only these explicit essential keys cross into the
	// harness; device API keys, daemon credentials and arbitrary ambient values
	// are excluded. Operator-declared environment remains intentional authority.
	essential := essentialEnvironment(os.LookupEnv)
	return OwnerContext{UID: uid, Name: owner.Username, Home: owner.HomeDir, Hostname: hostname}, essential, nil
}

func essentialEnvironment(lookup func(string) (string, bool)) map[string]string {
	essential := map[string]string{}
	for _, key := range []string{"PATH", "SHELL", "TERM", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if value, exists := lookup(key); exists {
			essential[key] = value
		}
	}
	if essential["TERM"] == "" {
		essential["TERM"] = "xterm-256color"
	}
	return essential
}

func (h *holder) writeState() bool {
	h.state.UpdatedAt = time.Now().UTC()
	h.statePending = true
	if len(h.consolePending) != 0 || len(h.eventsPending) != 0 || h.markerPending != nil {
		return true // Accepted in memory, not a durable terminal acknowledgment.
	}
	err := h.session.WriteState(h.state)
	if errors.Is(err, ErrBusy) {
		return true // Retry without killing the harness or claiming corruption.
	}
	if err != nil {
		h.storageFailed = true
		return false
	}
	h.statePending = false
	return true
}

func (h *holder) event(kind, reason, viewer, role string) {
	event := Event{Kind: kind, Reason: reason, At: time.Now().UTC(), Incarnation: h.state.Incarnation, ChildGeneration: h.state.ChildGeneration, ViewerID: viewer, Role: role}
	// Intake pauses at half this cap, leaving room for one actor transition's
	// bounded viewer-detach fanout and cleanup events. Never grow without bound.
	if len(h.eventsPending) >= maxPendingEvents {
		h.quarantined = true
		h.controller, h.controlPending = "", true
		h.epoch++
		if h.terminal != nil {
			h.terminal.epoch.Store(h.epoch)
		}
		h.state.Phase = "event_queue_overflow"
		h.statePending = true
		return
	}
	if len(h.consolePending) != 0 || len(h.eventsPending) != 0 {
		h.eventsPending = append(h.eventsPending, event)
		return
	}
	err := h.logs.AppendEvent(event)
	if errors.Is(err, ErrBusy) {
		h.eventsPending = append(h.eventsPending, event)
	} else if err != nil {
		h.storageFailed = true
	}
}

// flushPending is an actor operation. Busy means no writer acquired the lock,
// so retain the exact bytes/events for a later tick. An actual I/O failure may
// be a partial append and must never be automatically replayed as durable data.
func (h *holder) flushPending() bool {
	if len(h.consolePending) != 0 {
		err := h.logs.AppendConsole(h.consolePending)
		if errors.Is(err, ErrBusy) {
			return false
		}
		h.consolePending = nil
		if err != nil {
			h.storageFailed = true
		}
	}
	for len(h.eventsPending) != 0 {
		err := h.logs.AppendEvent(h.eventsPending[0])
		if errors.Is(err, ErrBusy) {
			return false
		}
		h.eventsPending[0] = Event{}
		h.eventsPending = h.eventsPending[1:]
		if err != nil {
			h.storageFailed = true
		}
	}
	if h.markerPending != nil {
		err := h.session.WriteStopMarker(*h.markerPending)
		if errors.Is(err, ErrBusy) {
			return false
		}
		h.markerPending = nil
		if err != nil {
			h.storageFailed = true
		}
	}
	if h.statePending {
		if h.storageFailed && h.exitAfterFlush {
			h.state.Phase = "error"
		}
		err := h.session.WriteState(h.state)
		if errors.Is(err, ErrBusy) {
			return false
		}
		h.statePending = false
		if err != nil {
			h.storageFailed = true
		}
	}
	return true
}

func (h *holder) finishAfterFlush() bool {
	h.exitAfterFlush = true
	if !h.flushPending() {
		return false
	}
	h.finishStopReplies(h.finalError())
	return true
}

func (h *holder) hello() Hello {
	return Hello{ProtocolVersion: ProtocolVersion, BinaryVersion: h.state.BinaryVersion, Name: h.state.Name, Incarnation: h.state.Incarnation, Holder: h.state.Holder, Scope: h.state.Scope, DeclarationRevision: h.state.DeclarationRevision, ChildGeneration: h.state.ChildGeneration, Phase: h.state.Phase}
}

func (h *holder) startChild() error {
	if h.child != nil || h.cleaning || h.quarantined || h.stopRequested || h.storageFailed || h.scope.CheckMembers(h.ctx, []procscope.ProcessIdentity{toScopeProcess(h.state.Holder)}) != nil {
		return ErrUnavailable
	}
	// Environment bytes are frozen for the holder lifetime. Resolve executable
	// again per spawn, but never reread env_file or adopt an edited declaration.
	argv := append([]string(nil), h.declaration.Command...)
	if (h.restartCommand || h.state.ChildGeneration != 0) && len(h.declaration.CommandRestart) != 0 {
		argv = append([]string(nil), h.declaration.CommandRestart...)
	}
	path := ""
	for _, entry := range h.environment.Env {
		if value, found := strings.CutPrefix(entry, "PATH="); found {
			path = value
		}
	}
	executable, err := h.resolve(argv[0], h.declaration.Cwd, filepath.SplitList(path))
	if err != nil {
		return ErrUnavailable
	}
	argv[0] = executable
	h.state.ChildGeneration++
	child, err := h.spawn(h.declaration, argv, h.environment.Env, h.state.ChildGeneration)
	if child == nil {
		return ErrUnavailable
	}
	h.child = child
	h.state.Child = child.Identity()
	h.state.ResolvedExecutable = executable
	h.spawnAt = time.Now()
	if err != nil {
		// Do not start I/O workers on a partially configured descriptor. The
		// retained child still follows cleanup/quarantine, never a lost return.
		h.storageFailed = true
		h.readDone = make(chan struct{})
		close(h.readDone)
		h.beginCleanup(true)
		return err
	}
	readCtx, cancel := context.WithCancel(h.ctx)
	h.readCancel, h.readDone, h.readEOF = cancel, make(chan struct{}), false
	go readTerminal(readCtx, child, h.state.ChildGeneration, h.parts, h.readDone)
	h.terminal = newTerminalIO(h.ctx, child, h.state.ChildGeneration)
	h.controlPending, h.controller = false, ""
	h.epoch++
	h.terminal.epoch.Store(h.epoch)
	h.state.Phase = "running"
	if !h.writeState() {
		h.beginCleanup(true)
		return ErrUnavailable
	}
	event := Event{Kind: "spawn", At: time.Now().UTC(), Incarnation: h.state.Incarnation, ChildGeneration: h.state.ChildGeneration, ResolvedExecutable: executable, EnvironmentHash: h.state.EnvironmentHash}
	err = h.logs.AppendEvent(event)
	if errors.Is(err, ErrBusy) {
		h.eventsPending = append(h.eventsPending, event)
	} else if err != nil {
		h.storageFailed = true
		h.beginCleanup(true)
		return ErrUnavailable
	}
	return nil
}

func (h *holder) run(caller context.Context) error {
	callerDone := caller.Done()
	if h.startChild() != nil && h.child == nil {
		h.storageFailed = true
		h.state.Phase = "error"
		h.writeState()
		if h.finishAfterFlush() {
			return ErrUnavailable
		}
	}
	tick := time.NewTicker(consoleFlushInterval)
	defer tick.Stop()
	for {
		var exited <-chan struct{}
		if h.child != nil && !h.cleaning && !h.quarantined && !h.exitAfterFlush {
			exited = h.child.Exited()
		}
		parts, requests := h.parts, h.requests
		if h.outputPending != nil || len(h.consolePending) >= maxConsoleBatchBytes || len(h.eventsPending) >= maxPendingEvents/2 || h.exitAfterFlush {
			parts = nil // Bounded backpressure, never discard captured PTY output.
		}
		if len(h.eventsPending) >= maxPendingEvents/2 || h.exitAfterFlush {
			requests = nil
		}
		select {
		case <-callerDone:
			callerDone = nil // quarantine retains custody rather than busy-loop
			h.stopRequested = true
			h.beginCleanup(true)
		case <-exited:
			h.beginCleanup(false)
		case part := <-parts:
			if !h.output(part) {
				h.outputPending = &part
			}
		case request := <-requests:
			h.request(request)
		case control := <-h.controls:
			h.acceptControl(control)
		case result := <-h.cleaned:
			if h.finishCleanup(result) {
				return h.finalError()
			}
		case <-tick.C:
			if h.storageFailed && !h.cleaning && !h.quarantined && !h.exitAfterFlush {
				h.beginCleanup(true)
			}
			if !h.flushPending() {
				continue
			}
			if h.exitAfterFlush {
				h.finishStopReplies(h.finalError())
				return h.finalError()
			}
			if h.outputPending != nil {
				part := *h.outputPending
				h.outputPending = nil
				if !h.output(part) {
					h.outputPending = &part
				}
			}
			if h.cleanupPending != nil {
				result := *h.cleanupPending
				h.cleanupPending = nil
				if h.finishCleanup(result) {
					return h.finalError()
				}
			}
			if h.storageFailed && !h.cleaning && !h.quarantined {
				h.beginCleanup(true)
			}
			h.startDueChild(time.Now())
		}
	}
}

func (h *holder) startDueChild(now time.Time) {
	// Cleanup earlier in this same tick can queue records and consume the
	// backoff interval while waiting for their locks. A due clock alone never
	// authorizes the next generation to overtake those lifecycle publications.
	if h.nextSpawn.IsZero() || now.Before(h.nextSpawn) || h.child != nil || h.cleaning || h.quarantined || h.stopRequested || h.storageFailed || h.exitAfterFlush ||
		len(h.consolePending) != 0 || len(h.eventsPending) != 0 || h.markerPending != nil || h.statePending || h.cleanupPending != nil || h.outputPending != nil {
		return
	}
	h.nextSpawn = time.Time{}
	if h.startChild() != nil && h.child == nil {
		h.quarantined = true
		h.state.Phase = "spawn_failed"
		h.writeState()
	}
}

func (h *holder) output(part outputPart) bool {
	if part.generation != h.state.ChildGeneration {
		return true
	}
	if len(part.data) != 0 {
		if len(h.consolePending)+len(part.data) > maxConsoleBatchBytes && !h.flushPending() {
			return false
		}
		h.consolePending = append(h.consolePending, part.data...)
		h.publishOutput(part.data)
		if len(h.consolePending) >= maxConsoleBatchBytes {
			h.flushPending()
		}
	}
	if part.err != nil {
		h.readEOF = isTerminalEOF(part.err)
		if !h.readEOF {
			h.storageFailed = true
		}
	}
	return true
}

func (h *holder) finalError() error {
	if h.storageFailed || h.quarantined {
		return ErrUnavailable
	}
	return nil
}

func toScopeProcess(p ProcessIdentity) procscope.ProcessIdentity {
	return procscope.ProcessIdentity{PID: p.PID, StartTicks: p.StartTicks, UID: p.UID, BootID: p.BootID}
}

func fromScopeProcess(p procscope.ProcessIdentity) ProcessIdentity {
	return ProcessIdentity{PID: p.PID, StartTicks: p.StartTicks, UID: p.UID, BootID: p.BootID}
}

func filepathAbsolute(path string) bool { return filepath.IsAbs(path) }
