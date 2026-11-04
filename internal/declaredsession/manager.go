package declaredsession

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/procscope"
)

var ErrManager = errors.New("declared session ownership or lifecycle unconfirmed")

// ManagedStatus is a local observation, never a permission to spawn or signal.
type ManagedStatus struct {
	Name           string `json:"name"`
	Classification string `json:"classification"`
	State          State  `json:"state"`
	StopMarker     bool   `json:"stop_marker"`
}

type managerPeer interface {
	Hello() Hello
	Status(context.Context) (State, error)
	Stop(context.Context, string) error
	PrepareRestart(context.Context, string) error
	Close() error
}

type managerSocket struct {
	Present bool
	Device  uint64
	Inode   uint64
}

type managerOps struct {
	inspect      func(context.Context, string) (procscope.Observation, error)
	process      func(int) (procscope.ProcessIdentity, error)
	stop         func(context.Context, procscope.ScopeIdentity, procscope.ProcessIdentity) error
	dial         func(context.Context, *SessionStore) (managerPeer, error)
	attachDial   func(context.Context, *SessionStore) (*Client, error)
	socket       func(*SessionStore) (managerSocket, error)
	removeSocket func(*SessionStore, managerSocket) error
	launch       func(context.Context, string, string, string, string, string, bool) (int, error)
}

type Manager struct {
	store      *Store
	executable string
	version    string
	ops        managerOps
}

func NewManager(store *Store, executable, version string) (*Manager, error) {
	if store == nil || !filepath.IsAbs(executable) || version == "" || len(version) > 128 {
		return nil, ErrManager
	}
	return &Manager{store: store, executable: executable, version: version, ops: nativeManagerOps()}, nil
}

// ScopeName preserves the canonical declaration exactly, including dots. It
// does not share tmux's name grammar or randomly named scope namespace.
func ScopeName(name string) (string, error) {
	if ValidateName(name) != nil {
		return "", ErrConfig
	}
	return "citadel-sess-" + name + ".scope", nil
}

func (m *Manager) declaration(name string) (Snapshot, Declaration, error) {
	if m == nil || m.store == nil || ValidateName(name) != nil {
		return Snapshot{}, Declaration{}, ErrConfig
	}
	snapshot, err := m.store.LoadConfig()
	if err != nil {
		return Snapshot{}, Declaration{}, err
	}
	declaration, found := snapshot.Lookup(name)
	if !found {
		return Snapshot{}, Declaration{}, ErrConfig
	}
	return snapshot, declaration, nil
}

func processToScope(value ProcessIdentity) procscope.ProcessIdentity {
	return procscope.ProcessIdentity{PID: value.PID, StartTicks: value.StartTicks, UID: value.UID, BootID: value.BootID}
}

func scopeFromState(value State) procscope.ScopeIdentity {
	return procscope.ScopeIdentity{Name: value.Scope.Name, InvocationID: value.Scope.InvocationID, ControlGroup: value.Scope.ControlGroup, UID: value.Holder.UID}
}

func validProcessIdentity(value ProcessIdentity) bool {
	return value.PID > 0 && value.StartTicks != 0 && value.UID != 0 && value.BootID != ""
}

func containsScopeProcess(members []procscope.ProcessIdentity, expected ProcessIdentity) bool {
	if !validProcessIdentity(expected) {
		return false
	}
	for _, member := range members {
		if member == processToScope(expected) {
			return true
		}
	}
	return false
}

// The disk record is correlated with an authenticated socket AND live scope
// membership. Possession of a state/PID file is not holder authority.
func sameHolderRecord(stored State, hello Hello, live State, scope procscope.Observation) bool {
	return validProcessIdentity(live.Holder) && stored.Incarnation == hello.Incarnation && stored.Incarnation == live.Incarnation &&
		stored.Name == hello.Name && stored.Name == live.Name && stored.Holder == hello.Holder && stored.Holder == live.Holder &&
		stored.Scope == hello.Scope && stored.Scope == live.Scope && stored.DeclarationRevision == hello.DeclarationRevision && stored.DeclarationRevision == live.DeclarationRevision &&
		stored.ProtocolVersion == ProtocolVersion && hello.ProtocolVersion == ProtocolVersion && live.ProtocolVersion == ProtocolVersion &&
		stored.BinaryVersion == hello.BinaryVersion && stored.BinaryVersion == live.BinaryVersion &&
		scope.Scope == scopeFromState(live) && containsScopeProcess(scope.Members, live.Holder)
}

func (m *Manager) observe(ctx context.Context, declaration Declaration, session *SessionStore) (ManagedStatus, managerSocket, error) {
	result := ManagedStatus{Name: declaration.Name, Classification: "not_started"}
	unit, _ := ScopeName(declaration.Name)
	observation, err := m.ops.inspect(ctx, unit)
	if err != nil {
		result.Classification = "isolation_unavailable"
		return result, managerSocket{}, ErrManager
	}
	if session == nil {
		if observation.Scope.Name != "" || len(observation.Members) != 0 {
			result.Classification = "holder_lost"
			return result, managerSocket{}, ErrManager
		}
		if !declaration.Enabled {
			result.Classification = "disabled"
		}
		return result, managerSocket{}, nil
	}
	stored, haveState, err := session.ReadState()
	if err != nil {
		result.Classification = "holder_lost"
		return result, managerSocket{}, ErrManager
	}
	result.State = stored
	marker, haveMarker, err := session.ReadStopMarker()
	if err != nil {
		result.Classification = "config_conflict"
		return result, managerSocket{}, ErrManager
	}
	result.StopMarker = haveMarker
	socket, err := m.ops.socket(session)
	if err != nil {
		result.Classification = "holder_lost"
		return result, managerSocket{}, ErrManager
	}
	if socket.Present {
		peer, err := m.ops.dial(ctx, session)
		if err == nil {
			defer peer.Close()
			live, liveErr := peer.Status(ctx)
			if liveErr == nil && haveState && sameHolderRecord(stored, peer.Hello(), live, observation) {
				result.State, result.Classification = live, live.Phase
				if haveMarker {
					result.Classification = "stopping"
					if marker.DeclarationRevision != declaration.Revision {
						result.Classification = "config_conflict"
					}
				} else if live.DeclarationRevision != declaration.Revision {
					result.Classification = "config_drift"
				} else if live.BinaryVersion != m.version {
					result.Classification = "holder_outdated"
				}
				return result, socket, nil
			}
		}
		result.Classification = "holder_lost"
		return result, socket, ErrManager
	}
	if observation.Scope.Name != "" || len(observation.Members) != 0 {
		result.Classification = "holder_lost"
		return result, socket, ErrManager
	}
	if haveState && validProcessIdentity(stored.Holder) {
		live, err := m.ops.process(stored.Holder.PID)
		if !errors.Is(err, procscope.ErrProcessGone) && (err != nil || live == processToScope(stored.Holder)) {
			result.Classification = "holder_lost"
			return result, socket, ErrManager
		}
	}
	if haveMarker {
		result.Classification = "stopped"
		if haveState && emptyHolderStartingReservation(stored) && marker.Incarnation == stored.Incarnation && marker.DeclarationRevision == stored.DeclarationRevision {
			result.Classification = "recovery_incomplete"
		}
		if stored.Phase == "stopped_clean" {
			result.Classification = "stopped_clean"
		}
		if marker.DeclarationRevision != declaration.Revision {
			result.Classification = "config_conflict"
		}
	} else if !declaration.Enabled {
		result.Classification = "disabled"
	} else if haveState {
		result.Classification = "stopped"
		if stored.Phase == "isolation_unavailable" {
			result.Classification = "isolation_unavailable"
		}
		if stored.Phase == "starting" {
			// A launcher may not have reached its scope yet. Losing the caller
			// or its readiness deadline is not evidence that it cannot exec.
			result.Classification = "starting"
			return result, socket, ErrManager
		}
	}
	return result, socket, nil
}

func (m *Manager) Status(ctx context.Context, name string) (ManagedStatus, error) {
	_, declaration, err := m.declaration(name)
	if err != nil {
		return ManagedStatus{Name: name, Classification: "config_error"}, err
	}
	session, err := m.store.OpenSession(name, false)
	if errors.Is(err, os.ErrNotExist) {
		status, _, err := m.observe(ctx, declaration, nil)
		return status, err
	}
	if err != nil {
		return ManagedStatus{Name: name, Classification: "holder_lost"}, ErrManager
	}
	defer session.Close()
	status, _, err := m.observe(ctx, declaration, session)
	return status, err
}

func (m *Manager) Inventory(ctx context.Context) ([]ManagedStatus, error) {
	if m == nil || m.store == nil {
		return nil, ErrManager
	}
	snapshot, err := m.store.LoadConfig()
	if err != nil {
		return nil, err
	}
	result := make([]ManagedStatus, 0, len(snapshot.Sessions))
	var failures []error
	for _, declaration := range snapshot.Sessions {
		status, err := m.Status(ctx, declaration.Name)
		result = append(result, status)
		if err != nil {
			failures = append(failures, err)
		}
	}
	return result, errors.Join(failures...)
}

func (m *Manager) Start(ctx context.Context, name string) (ManagedStatus, error) {
	return m.change(ctx, name, false)
}

func (m *Manager) Restart(ctx context.Context, name string) (ManagedStatus, error) {
	return m.change(ctx, name, true)
}

func (m *Manager) change(ctx context.Context, name string, restart bool) (ManagedStatus, error) {
	snapshot, declaration, err := m.declaration(name)
	if err != nil || !declaration.Enabled {
		return ManagedStatus{Name: name, Classification: "disabled"}, ErrConfig
	}
	session, err := m.store.OpenSession(name, true)
	if err != nil {
		return ManagedStatus{Name: name, Classification: "holder_lost"}, err
	}
	defer session.Close()
	guard, err := session.AcquireOperation(ctx)
	if err != nil {
		return ManagedStatus{Name: name, Classification: "starting"}, err
	}
	defer guard.Close()
	checked, current, err := m.declaration(name)
	if err != nil || checked.FileRevision != snapshot.FileRevision || current.Revision != declaration.Revision {
		return ManagedStatus{Name: name, Classification: "config_conflict"}, ErrConflict
	}
	status, socket, observeErr := m.observe(ctx, declaration, session)
	if restart {
		if err := m.stopLocked(ctx, session, declaration, status, socket, true); err != nil {
			return status, err
		}
		status, _, observeErr = m.observe(ctx, declaration, session)
	}
	// A stop marker may have become durable immediately before a recovery
	// state write failed. Only an explicit Stop retry may finish retiring this
	// reservation; Start must not clear its veto while a late launcher can
	// still recognize the old starting record.
	if status.State.Phase == "starting" && !validProcessIdentity(status.State.Holder) {
		return status, ErrManager
	}
	if observeErr != nil {
		return status, observeErr
	}
	if validProcessIdentity(status.State.Holder) {
		if _, err := m.ops.process(status.State.Holder.PID); err == nil && status.Classification != "stopped" && status.Classification != "stopped_clean" && status.Classification != "config_conflict" {
			return status, nil // Existing authenticated holder is never restarted by Start.
		}
	}
	if status.Classification == "running" || status.Classification == "restart_wait" || status.Classification == "starting" || status.Classification == "config_drift" || status.Classification == "holder_outdated" {
		return status, nil
	}
	// A terminal RPC phase or an available owner lock does not prove that the
	// holder has finished tearing down. Reclassify actual scope and old birth
	// before clearing a marker, replacing state, or launching another process.
	unit, _ := ScopeName(name)
	remaining, err := m.ops.inspect(ctx, unit)
	if err != nil || remaining.Scope.Name != "" || len(remaining.Members) != 0 {
		return status, ErrManager
	}
	if validProcessIdentity(status.State.Holder) {
		old, err := m.ops.process(status.State.Holder.PID)
		if !errors.Is(err, procscope.ErrProcessGone) && (err != nil || old == processToScope(status.State.Holder)) {
			return status, ErrManager
		}
	}
	owner, err := session.AcquireOwner(ctx)
	if err != nil {
		return ManagedStatus{Name: name, Classification: "holder_lost"}, ErrManager
	}
	// A last-known stop marker vetoes by NAME, even after a YAML revision edit.
	// Only this explicit local Start/Restart, after the gone proof above, clears it.
	marker, present, err := session.ReadStopMarker()
	if err == nil && present {
		err = session.ClearStopMarker(guard, marker)
	}
	if closeErr := owner.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return status, ErrManager
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return status, ErrManager
	}
	incarnation := hex.EncodeToString(nonce[:])
	pending := State{Version: 1, Name: name, Incarnation: incarnation, Scope: ScopeIdentity{Name: "citadel-sess-" + name + ".scope", Manager: "user"}, ProtocolVersion: ProtocolVersion, BinaryVersion: m.version, FileRevision: snapshot.FileRevision, DeclarationRevision: declaration.Revision, Phase: "starting", Guarantee: "unconfirmed", StartedAt: time.Now(), UpdatedAt: time.Now()}
	if session.WriteState(pending) != nil {
		return status, ErrManager
	}
	pid, err := m.ops.launch(ctx, m.executable, m.store.NodeDir(), name, incarnation, m.version, restart)
	if err != nil {
		// The native launcher returns zero only when no child was started.
		// Retire that reservation only after corroborating the exact pending
		// record and empty scope. A positive PID, changed record or unknown
		// observation retains custody in starting instead of allowing retry.
		if pid == 0 {
			if failed, retireErr := m.retireUnlaunched(ctx, session, pending); retireErr == nil {
				return ManagedStatus{Name: name, Classification: "isolation_unavailable", State: failed}, ErrManager
			}
		}
		return ManagedStatus{Name: name, Classification: "starting", State: pending}, ErrManager
	}
	launcher, launcherErr := m.ops.process(pid)
	if launcherErr != nil || launcher.PID != pid || recordManagerLaunch(session, incarnation, launcher) != nil {
		return ManagedStatus{Name: name, Classification: "starting", State: pending}, ErrManager
	}
	// Start() of the launcher is not a scope/readiness success. Wait only for
	// this exact incarnation's authenticated holder and independently live scope.
	waitCtx, cancel := context.WithTimeout(ctx, operationWait)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		observed, _, err := m.observe(waitCtx, declaration, session)
		if err == nil && observed.State.Incarnation == incarnation && observed.State.Holder.PID == pid && validProcessIdentity(observed.State.Holder) {
			return observed, nil
		}
		select {
		case <-waitCtx.Done():
			return ManagedStatus{Name: name, Classification: "starting", State: pending}, ErrManager
		case <-ticker.C:
		}
	}
}

func (m *Manager) retireUnlaunched(ctx context.Context, session *SessionStore, expected State) (State, error) {
	// Finish the bounded no-launch proof even if the operator cancelled just
	// before exec. Cancellation alone never proves that a process is absent.
	proofCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationWait)
	defer cancel()
	observation, err := m.ops.inspect(proofCtx, expected.Scope.Name)
	if err != nil || observation.Scope != (procscope.ScopeIdentity{}) || len(observation.Members) != 0 {
		return State{}, ErrManager
	}
	var retired State
	err = session.withRecord(func() error {
		current, present, err := session.ReadState()
		if err != nil || !present || current.Incarnation != expected.Incarnation || current.Name != expected.Name ||
			current.Scope != expected.Scope || current.DeclarationRevision != expected.DeclarationRevision || current.FileRevision != expected.FileRevision ||
			current.ProtocolVersion != expected.ProtocolVersion || current.BinaryVersion != expected.BinaryVersion || current.Phase != "starting" ||
			current.Guarantee != "unconfirmed" || current.Holder != (ProcessIdentity{}) || current.Child != (ProcessIdentity{}) ||
			current.Scope.InvocationID != "" || current.Scope.ControlGroup != "" {
			return ErrManager
		}
		current.Phase, current.UpdatedAt = "isolation_unavailable", time.Now()
		if err := session.writeJSON("state.json", current, MaxStateBytes); err != nil {
			return ErrManager
		}
		retired = current
		return nil
	})
	return retired, err
}

// The record lock serializes this one transition with the holder's own state
// publication. A read-then-WriteState outside it could overwrite a READY state.
func recordManagerLaunch(session *SessionStore, incarnation string, launcher procscope.ProcessIdentity) error {
	if launcher.PID <= 0 || launcher.StartTicks == 0 || launcher.UID == 0 || launcher.BootID == "" {
		return ErrManager
	}
	return session.withRecord(func() error {
		state, present, err := session.ReadState()
		if err != nil || !present || state.Incarnation != incarnation {
			return ErrManager
		}
		if state.Holder.PID != 0 || state.Phase != "starting" {
			return nil // The authenticated holder will be independently checked.
		}
		state.Holder = ProcessIdentity{PID: launcher.PID, StartTicks: launcher.StartTicks, UID: launcher.UID, BootID: launcher.BootID}
		state.UpdatedAt = time.Now()
		if session.validateState(state) != nil {
			return ErrManager
		}
		return session.writeJSON("state.json", state, MaxStateBytes)
	})
}

// Dial validates and returns the SAME authenticated connection. It does not
// observe one holder and then give the CLI an unchecked replacement dial.
func (m *Manager) Dial(ctx context.Context, name string) (*Client, error) {
	_, declaration, err := m.declaration(name)
	if err != nil || declaration.Attach.Local == "deny" {
		return nil, ErrManager
	}
	session, err := m.store.OpenSession(name, false)
	if err != nil {
		return nil, ErrManager
	}
	defer session.Close()
	guard, err := session.AcquireOperation(ctx)
	if err != nil {
		return nil, ErrManager
	}
	defer guard.Close()
	_, current, err := m.declaration(name)
	if err != nil || current.Revision != declaration.Revision {
		return nil, ErrConflict
	}
	stored, present, err := session.ReadState()
	if err != nil || !present {
		return nil, ErrManager
	}
	// Production Dial returns a concrete Client so the CLI can attach; the
	// operation does not expose the manager's injected test-peer abstraction.
	client, err := m.ops.attachDial(ctx, session)
	if err != nil {
		return nil, err
	}
	live, statusErr := client.Status(ctx)
	unit, _ := ScopeName(name)
	observation, observeErr := m.ops.inspect(ctx, unit)
	if statusErr != nil || observeErr != nil || !attachmentMatches(declaration, stored, client.Hello(), live, observation) {
		_ = client.Close()
		return nil, ErrManager
	}
	return client, nil
}

// A newer CLI can attach to an older holder that speaks this protocol. Binary
// versions must still corroborate the same holder across disk/hello/live state;
// they are not compared to the CLI release. Status reports holder_outdated.
func attachmentMatches(declaration Declaration, stored State, hello Hello, live State, observation procscope.Observation) bool {
	return sameHolderRecord(stored, hello, live, observation) && live.DeclarationRevision == declaration.Revision && live.Guarantee == "verified_user_scope"
}

func (m *Manager) Stop(ctx context.Context, name string) (ManagedStatus, error) {
	_, declaration, err := m.declaration(name)
	if err != nil {
		return ManagedStatus{Name: name, Classification: "config_error"}, err
	}
	session, err := m.store.OpenSession(name, false)
	if err != nil {
		return ManagedStatus{Name: name, Classification: "holder_lost"}, ErrManager
	}
	defer session.Close()
	guard, err := session.AcquireOperation(ctx)
	if err != nil {
		return ManagedStatus{Name: name, Classification: "holder_lost"}, err
	}
	defer guard.Close()
	status, socket, _ := m.observe(ctx, declaration, session)
	if err := m.stopLocked(ctx, session, declaration, status, socket, false); err != nil {
		return status, err
	}
	status, _, err = m.observe(ctx, declaration, session)
	return status, err
}

func (m *Manager) stopLocked(ctx context.Context, session *SessionStore, declaration Declaration, status ManagedStatus, socket managerSocket, restarting bool) error {
	stored, exists, err := session.ReadState()
	if err != nil {
		return ErrManager
	}
	if !exists {
		if status.Classification == "not_started" {
			return nil
		}
		return ErrManager
	}
	if !validProcessIdentity(stored.Holder) {
		if stored.Phase == "starting" {
			if restarting {
				return ErrManager // Recovery is explicit Stop, followed by Start.
			}
			return m.retireHolderlessStarting(ctx, session, stored)
		}
		if status.Classification == "stopped" || status.Classification == "stopped_clean" || status.Classification == "config_conflict" ||
			status.Classification == "isolation_unavailable" && stored.Phase == "isolation_unavailable" && stored.Holder == (ProcessIdentity{}) {
			return nil
		}
		return ErrManager
	}
	unit, _ := ScopeName(declaration.Name)
	observation, err := m.ops.inspect(ctx, unit)
	if err != nil {
		return ErrManager
	}
	alreadyGone := observation.Scope.Name == "" && len(observation.Members) == 0
	if alreadyGone {
		live, err := m.ops.process(stored.Holder.PID)
		if !errors.Is(err, procscope.ErrProcessGone) && (err != nil || live == processToScope(stored.Holder)) {
			return ErrManager
		}
	} else if observation.Scope != scopeFromState(stored) {
		return ErrManager
	}
	marker, haveMarker, err := session.ReadStopMarker()
	if err != nil {
		return ErrManager
	}
	if !haveMarker {
		marker = StopMarker{Version: 1, Name: declaration.Name, DeclarationRevision: stored.DeclarationRevision, Incarnation: stored.Incarnation, StoppedAt: time.Now()}
		if session.WriteStopMarker(marker) != nil {
			return ErrManager
		}
	}
	graceful := false
	if peer, err := m.ops.dial(ctx, session); err == nil {
		hello := peer.Hello()
		live, statusErr := peer.Status(ctx)
		if alreadyGone || statusErr != nil || !sameHolderRecord(stored, hello, live, observation) {
			_ = peer.Close()
			return ErrManager
		}
		if live.Phase == "cleanup_unconfirmed" || live.Phase == "spawn_failed" || live.Phase == "event_queue_overflow" {
			// An authenticated quarantined holder cannot promise graceful
			// child cleanup. Explicit local stop/restart proceeds only through
			// the same saved exact invocation, without a flush/ACK claim.
			_ = peer.Close()
		} else {
			if restarting {
				err = peer.PrepareRestart(ctx, stored.Incarnation)
			} else {
				err = peer.Stop(ctx, stored.Incarnation)
			}
			_ = peer.Close()
			if err != nil {
				return ErrManager
			}
			// The holder ACKs only after synchronous log/state durability and child
			// cleanup. Still wait for this exact process birth to disappear; killing
			// the scope immediately after an ACK could interrupt holder teardown.
			if m.waitGracefulExit(ctx, session, stored) != nil {
				return ErrManager
			}
			graceful = true
		}
	}
	// Lost-holder recovery is forced exact-scope cleanup, not evidence of log
	// flush. A healthy holder is never forced before its completion is confirmed.
	if !alreadyGone && m.ops.stop(ctx, scopeFromState(stored), processToScope(stored.Holder)) != nil {
		return ErrManager
	}
	owner, err := session.AcquireOwner(ctx)
	if err != nil {
		return ErrManager
	}
	defer owner.Close()
	if socket.Present && m.ops.removeSocket(session, socket) != nil {
		return ErrManager
	}
	// A pending launcher whose exact birth and scope are now absent can be
	// explicitly retired. Do not leave a forever-starting record or overwrite
	// some later incarnation's state after a raced replacement.
	return session.withRecord(func() error {
		current, present, err := session.ReadState()
		if err != nil || !present || current.Incarnation != stored.Incarnation {
			return ErrManager
		}
		if graceful {
			if current.Phase != "stopped" && current.Phase != "stopped_clean" {
				return ErrManager
			}
			return nil // Preserve the holder's durable completion record.
		}
		current.Phase, current.Guarantee, current.UpdatedAt = "stopped", "forced_cleanup_no_flush", time.Now()
		return session.writeJSON("state.json", current, MaxStateBytes)
	})
}

// The caller retains OperationGuard. OwnerGuard is acquired BEFORE inspecting
// the exact saved scope, fencing a delayed holder's own owner-lock/publication
// path. A scope/PID filename alone never authorizes retirement or signalling.
func (m *Manager) retireHolderlessStarting(ctx context.Context, session *SessionStore, expected State) (retErr error) {
	if !emptyHolderStartingReservation(expected) || ctx.Err() != nil {
		return ErrManager
	}
	owner, err := session.AcquireOwner(ctx)
	if err != nil {
		return ErrManager
	}
	defer func() {
		if owner.Close() != nil {
			retErr = ErrManager
		}
	}()
	observation, err := m.ops.inspect(ctx, expected.Scope.Name)
	if err != nil || ctx.Err() != nil || observation.Scope != (procscope.ScopeIdentity{}) || len(observation.Members) != 0 {
		return ErrManager
	}
	// No legitimate delayed holder can publish a socket while OwnerGuard is
	// held. An unexplained existing socket is retained rather than unlinked.
	socket, err := m.ops.socket(session)
	if err != nil || socket.Present || ctx.Err() != nil {
		return ErrManager
	}
	return session.withRecord(func() error {
		current, present, err := session.ReadState()
		if err != nil || !present || ctx.Err() != nil || !sameEmptyStartingReservation(current, expected) {
			return ErrManager
		}
		marker, haveMarker, err := session.ReadStopMarker()
		if err != nil || haveMarker && (marker.Incarnation != expected.Incarnation || marker.DeclarationRevision != expected.DeclarationRevision) {
			return ErrManager
		}
		if !haveMarker {
			marker = StopMarker{Version: 1, Name: current.Name, DeclarationRevision: current.DeclarationRevision, Incarnation: current.Incarnation, StoppedAt: time.Now()}
			// The same-name veto is durable first. A failure before the terminal
			// record leaves starting held; retrying Stop completes this sequence.
			if session.writeJSON("stop.json", marker, MaxStateBytes) != nil {
				return ErrManager
			}
		}
		current.Phase, current.Guarantee, current.UpdatedAt = "stopped", "forced_cleanup_no_flush", time.Now()
		return session.writeJSON("state.json", current, MaxStateBytes)
	})
}

func emptyHolderStartingReservation(state State) bool {
	return state.Version == 1 && ValidateName(state.Name) == nil && validIncarnation(state.Incarnation) &&
		validDigest(state.DeclarationRevision) && validDigest(state.FileRevision) && (state.ProtocolVersion == 1 || state.ProtocolVersion == ProtocolVersion) &&
		state.BinaryVersion != "" && len(state.BinaryVersion) <= 128 && state.Phase == "starting" && state.Guarantee == "unconfirmed" &&
		state.Scope == (ScopeIdentity{Name: "citadel-sess-" + state.Name + ".scope", Manager: "user"}) &&
		state.Holder == (ProcessIdentity{}) && state.Child == (ProcessIdentity{}) && state.RestartCount == 0 && state.ChildGeneration == 0 &&
		state.LastExit == nil && state.ResolvedExecutable == "" && state.EnvironmentHash == "" && !state.StartedAt.IsZero() && !state.UpdatedAt.IsZero()
}

func sameEmptyStartingReservation(current, expected State) bool {
	return emptyHolderStartingReservation(current) && emptyHolderStartingReservation(expected) &&
		current.Incarnation == expected.Incarnation && current.Name == expected.Name && current.Scope == expected.Scope &&
		current.DeclarationRevision == expected.DeclarationRevision && current.FileRevision == expected.FileRevision &&
		current.ProtocolVersion == expected.ProtocolVersion && current.BinaryVersion == expected.BinaryVersion &&
		current.StartedAt.Equal(expected.StartedAt) && current.UpdatedAt.Equal(expected.UpdatedAt)
}

func (m *Manager) waitGracefulExit(ctx context.Context, session *SessionStore, expected State) error {
	waitCtx, cancel := context.WithTimeout(ctx, operationWait)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		live, err := m.ops.process(expected.Holder.PID)
		gone := errors.Is(err, procscope.ErrProcessGone) || (err == nil && live != processToScope(expected.Holder))
		if err != nil && !errors.Is(err, procscope.ErrProcessGone) {
			return ErrManager
		}
		if gone {
			final, present, err := session.ReadState()
			if err != nil || !present || final.Incarnation != expected.Incarnation || final.Holder != expected.Holder ||
				(final.Phase != "stopped" && final.Phase != "stopped_clean") {
				return ErrManager
			}
			return nil
		}
		select {
		case <-waitCtx.Done():
			return ErrManager
		case <-ticker.C:
		}
	}
}
