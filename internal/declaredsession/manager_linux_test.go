//go:build linux

package declaredsession

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/procscope"
)

type fakeManagerPeer struct {
	hello   Hello
	state   State
	onStop  func() error
	closed  bool
	restart bool
}

func (p *fakeManagerPeer) Hello() Hello { return p.hello }
func (p *fakeManagerPeer) Status(context.Context) (State, error) {
	return p.state, nil
}
func (p *fakeManagerPeer) Stop(context.Context, string) error {
	return p.onStop()
}
func (p *fakeManagerPeer) PrepareRestart(context.Context, string) error {
	p.restart = true
	return p.onStop()
}
func (p *fakeManagerPeer) Close() error {
	p.closed = true
	return nil
}

func TestManagerReadinessTimeoutDoesNotAuthorizeDuplicate(t *testing.T) {
	_, session := fixtureStore(t)
	state := fixtureState()
	state.Phase = "starting"
	if session.WriteState(state) != nil {
		t.Fatal("fixture state failed")
	}
	m := &Manager{ops: managerOps{
		inspect: func(context.Context, string) (procscope.Observation, error) {
			return procscope.Observation{}, nil
		},
		socket: func(*SessionStore) (managerSocket, error) { return managerSocket{}, nil },
	}}
	status, _, err := m.observe(context.Background(), Declaration{Name: "fixture", Enabled: true}, session)
	if !errors.Is(err, ErrManager) || status.Classification != "starting" {
		t.Fatal("absent-yet-pending launch was treated as restartable")
	}
}

func TestManagerAckDoesNotKillHolderBeforeActualExit(t *testing.T) {
	_, session := fixtureStore(t)
	state, hello, observed := managerRecordFixture()
	if session.WriteState(state) != nil {
		t.Fatal("fixture state failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	peer := &fakeManagerPeer{hello: hello, state: state, onStop: func() error {
		cancel() // ACK arrived, but this holder has not actually exited.
		return nil
	}}
	stops := 0
	m := &Manager{ops: managerOps{
		inspect: func(context.Context, string) (procscope.Observation, error) { return observed, nil },
		process: func(int) (procscope.ProcessIdentity, error) { return processToScope(state.Holder), nil },
		dial:    func(context.Context, *SessionStore) (managerPeer, error) { return peer, nil },
		stop: func(context.Context, procscope.ScopeIdentity, procscope.ProcessIdentity) error {
			stops++
			return nil
		},
	}}
	err := m.stopLocked(ctx, session, Declaration{Name: state.Name, Revision: state.DeclarationRevision}, ManagedStatus{}, managerSocket{}, false)
	if !errors.Is(err, ErrManager) || stops != 0 || !peer.closed {
		t.Fatal("RPC ACK became forced cleanup or graceful success before exit")
	}
	if marker, present, err := session.ReadStopMarker(); err != nil || !present || marker.Incarnation != state.Incarnation {
		t.Fatal("uncertain stop lost its durable veto")
	}
}

func TestManagerGracefulCompletionPrecedesResidualScopeCleanup(t *testing.T) {
	_, session := fixtureStore(t)
	state, hello, observed := managerRecordFixture()
	if session.WriteState(state) != nil {
		t.Fatal("fixture state failed")
	}
	completed := false
	peer := &fakeManagerPeer{hello: hello, state: state, onStop: func() error {
		final := state
		final.Phase = "stopped"
		if err := session.WriteState(final); err != nil {
			return err
		}
		completed = true // The fake completion ACK models synchronous durable publication.
		return nil
	}}
	stops := 0
	m := &Manager{ops: managerOps{
		inspect: func(context.Context, string) (procscope.Observation, error) { return observed, nil },
		process: func(int) (procscope.ProcessIdentity, error) {
			if completed {
				return procscope.ProcessIdentity{}, procscope.ErrProcessGone
			}
			return processToScope(state.Holder), nil
		},
		dial: func(context.Context, *SessionStore) (managerPeer, error) { return peer, nil },
		stop: func(context.Context, procscope.ScopeIdentity, procscope.ProcessIdentity) error {
			if !completed {
				t.Fatal("scope cleanup interrupted graceful completion")
			}
			stops++
			return nil
		},
	}}
	if err := m.stopLocked(context.Background(), session, Declaration{Name: state.Name, Revision: state.DeclarationRevision}, ManagedStatus{}, managerSocket{}, true); err != nil {
		t.Fatal(err)
	}
	final, _, err := session.ReadState()
	if err != nil || final.Phase != "stopped" || final.Guarantee != state.Guarantee || stops != 1 || !peer.restart {
		t.Fatal("completion record or explicit restart ordering changed")
	}
}

func TestManagerRequiresDurableTerminalRecordEvenAfterBirthGone(t *testing.T) {
	_, session := fixtureStore(t)
	state, _, _ := managerRecordFixture()
	if session.WriteState(state) != nil {
		t.Fatal("fixture state failed")
	}
	m := &Manager{ops: managerOps{process: func(int) (procscope.ProcessIdentity, error) {
		return procscope.ProcessIdentity{}, procscope.ErrProcessGone
	}}}
	if err := m.waitGracefulExit(context.Background(), session, state); !errors.Is(err, ErrManager) {
		t.Fatal("process exit alone became durable log/state completion proof")
	}
}

func TestManagerTerminalPhaseWithLivingHolderCannotStartReplacement(t *testing.T) {
	store, session := fixtureStore(t)
	raw := strings.Replace(fixtureConfig, "automated-citadel", "fixture", 1)
	if os.WriteFile(filepath.Join(store.NodeDir(), "sessions.yaml"), []byte(raw), 0600) != nil {
		t.Fatal("fixture config failed")
	}
	snapshot, err := store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	state, hello, observed := managerRecordFixture()
	if state.DeclarationRevision == snapshot.Sessions[0].Revision {
		t.Fatal("fixture must represent a changed declaration")
	}
	state.Phase = "stopped"
	hello.DeclarationRevision, hello.Phase = state.DeclarationRevision, state.Phase
	if session.WriteState(state) != nil {
		t.Fatal("fixture state failed")
	}
	marker := StopMarker{Version: 1, Name: state.Name, DeclarationRevision: state.DeclarationRevision, Incarnation: state.Incarnation, StoppedAt: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)}
	if session.WriteStopMarker(marker) != nil {
		t.Fatal("fixture stop marker failed")
	}
	peer := &fakeManagerPeer{hello: hello, state: state}
	m := &Manager{store: store, version: state.BinaryVersion, ops: managerOps{
		inspect: func(context.Context, string) (procscope.Observation, error) { return observed, nil },
		process: func(int) (procscope.ProcessIdentity, error) { return processToScope(state.Holder), nil },
		socket:  func(*SessionStore) (managerSocket, error) { return managerSocket{Present: true}, nil },
		dial:    func(context.Context, *SessionStore) (managerPeer, error) { return peer, nil },
		launch: func(context.Context, string, string, string, string, string, bool) (int, error) {
			t.Fatal("terminal phase or released owner lock allowed duplicate launch")
			return 0, ErrManager
		},
	}}
	if _, err := m.Start(context.Background(), "fixture"); !errors.Is(err, ErrManager) {
		t.Fatal("live terminal holder was treated as gone")
	}
	remaining, _, err := session.ReadState()
	if err != nil || remaining.Incarnation != state.Incarnation || remaining.Phase != state.Phase {
		t.Fatal("live holder state was replaced")
	}
	retained, present, err := session.ReadStopMarker()
	if err != nil || !present || retained.Version != marker.Version || retained.Name != marker.Name ||
		retained.DeclarationRevision != marker.DeclarationRevision || retained.Incarnation != marker.Incarnation || !retained.StoppedAt.Equal(marker.StoppedAt) {
		t.Fatal("live terminal holder's durable stop veto was cleared or changed")
	}
}

func writeManagerDeclarationFixture(t *testing.T, store *Store) {
	t.Helper()
	raw := strings.Replace(fixtureConfig, "automated-citadel", "fixture", 1)
	if err := os.WriteFile(filepath.Join(store.NodeDir(), "sessions.yaml"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestManagerFailedUnlaunchedReservationCanBeRetriedOrStopped(t *testing.T) {
	store, session := fixtureStore(t)
	writeManagerDeclarationFixture(t, store)
	launches := 0
	m := &Manager{store: store, executable: "/fixture/citadel", version: "fixture-version", ops: managerOps{
		inspect: func(context.Context, string) (procscope.Observation, error) {
			return procscope.Observation{}, nil
		},
		socket: func(*SessionStore) (managerSocket, error) { return managerSocket{}, nil },
		launch: func(context.Context, string, string, string, string, string, bool) (int, error) {
			launches++
			return 0, ErrManager // Pre-exec failure, no process started.
		},
	}}
	for _, retry := range []func(context.Context, string) (ManagedStatus, error){m.Start, m.Start, m.Restart} {
		status, err := retry(context.Background(), "fixture")
		if !errors.Is(err, ErrManager) || status.Classification != "isolation_unavailable" || status.State.Phase != "isolation_unavailable" {
			t.Fatalf("failed pre-exec reservation not retired: status=%+v err=%v", status, err)
		}
		stored, present, err := session.ReadState()
		if err != nil || !present || stored.Phase != "isolation_unavailable" || stored.Holder != (ProcessIdentity{}) {
			t.Fatal("failed launch left a permanent starting reservation")
		}
		if _, err := m.Stop(context.Background(), "fixture"); err != nil {
			t.Fatal("unlaunched terminal reservation cannot be stopped", err)
		}
	}
	if launches != 3 {
		t.Fatalf("explicit retries did not reach launcher: %d", launches)
	}
}

func TestManagerFailedLaunchWithResidualOrUnknownCustodyStaysHeld(t *testing.T) {
	for _, test := range []struct {
		name      string
		pid       int
		residual  bool
		unknown   bool
		birth     bool
		newRecord bool
	}{
		{name: "positive pid", pid: 42},
		{name: "residual scope", residual: true},
		{name: "unknown scope", unknown: true},
		{name: "captured birth", birth: true},
		{name: "replacement record", newRecord: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, session := fixtureStore(t)
			writeManagerDeclarationFixture(t, store)
			launches := 0
			launched := false
			m := &Manager{store: store, executable: "/fixture/citadel", version: "fixture-version", ops: managerOps{
				inspect: func(context.Context, string) (procscope.Observation, error) {
					if launched && test.unknown {
						return procscope.Observation{}, procscope.ErrUnknown
					}
					if launched && test.residual {
						_, _, observation := managerRecordFixture()
						return observation, nil
					}
					return procscope.Observation{}, nil
				},
				process: func(int) (procscope.ProcessIdentity, error) {
					state, _, _ := managerRecordFixture()
					return processToScope(state.Holder), nil
				},
				socket: func(*SessionStore) (managerSocket, error) { return managerSocket{}, nil },
				launch: func(context.Context, string, string, string, string, string, bool) (int, error) {
					launches++
					launched = true
					if test.birth || test.newRecord {
						current, _, err := session.ReadState()
						if err != nil {
							t.Fatal(err)
						}
						if test.birth {
							state, _, _ := managerRecordFixture()
							current.Holder = state.Holder
						}
						if test.newRecord {
							current.Incarnation = strings.Repeat("f", 32)
						}
						if err := session.WriteState(current); err != nil {
							t.Fatal(err)
						}
					}
					return test.pid, ErrManager
				},
			}}
			status, err := m.Start(context.Background(), "fixture")
			if !errors.Is(err, ErrManager) || status.Classification != "starting" {
				t.Fatal("uncertain failed launch was declared safely retired")
			}
			current, present, err := session.ReadState()
			if err != nil || !present || current.Phase != "starting" {
				t.Fatal("uncertain custody record was replaced by terminal state")
			}
			if _, err := m.Start(context.Background(), "fixture"); !errors.Is(err, ErrManager) || launches != 1 {
				t.Fatal("failed launch ambiguity permitted an automatic duplicate")
			}
		})
	}
}

func TestManagerRemainingScopeBlocksReplacementAfterOldBirthGone(t *testing.T) {
	for _, membersOnly := range []bool{false, true} {
		store, session := fixtureStore(t)
		writeManagerDeclarationFixture(t, store)
		state, _, observation := managerRecordFixture()
		state.Phase = "stopped"
		if err := session.WriteState(state); err != nil {
			t.Fatal(err)
		}
		inspections := 0
		m := &Manager{store: store, executable: "/fixture/citadel", version: "fixture-version", ops: managerOps{
			inspect: func(context.Context, string) (procscope.Observation, error) {
				inspections++
				if inspections == 1 {
					return procscope.Observation{}, nil // Initial terminal observation.
				}
				if membersOnly {
					return procscope.Observation{Members: observation.Members}, nil
				}
				return procscope.Observation{Scope: observation.Scope}, nil
			},
			process: func(int) (procscope.ProcessIdentity, error) {
				return procscope.ProcessIdentity{}, procscope.ErrProcessGone
			},
			socket: func(*SessionStore) (managerSocket, error) { return managerSocket{}, nil },
			launch: func(context.Context, string, string, string, string, string, bool) (int, error) {
				t.Fatal("replacement launched despite a remaining scope or member")
				return 0, ErrManager
			},
		}}
		if _, err := m.Start(context.Background(), "fixture"); !errors.Is(err, ErrManager) {
			t.Fatal("old birth disappearance alone permitted a replacement")
		}
		current, _, err := session.ReadState()
		if err != nil || current.Incarnation != state.Incarnation || current.Phase != state.Phase {
			t.Fatal("remaining-scope refusal overwrote the old terminal record")
		}
	}
}

func TestManagerExplicitStopCanForceExactQuarantinedScope(t *testing.T) {
	store, session := fixtureStore(t)
	writeManagerDeclarationFixture(t, store)
	state, hello, observation := managerRecordFixture()
	state.Phase, hello.Phase = "cleanup_unconfirmed", "cleanup_unconfirmed"
	if err := session.WriteState(state); err != nil {
		t.Fatal(err)
	}
	stopped, stopCalls, removed := false, 0, 0
	peer := &fakeManagerPeer{hello: hello, state: state, onStop: func() error {
		t.Fatal("quarantined holder was required to supply a graceful ACK")
		return ErrConflict
	}}
	socket := managerSocket{Present: true, Device: 1, Inode: 2}
	m := &Manager{store: store, version: "new-cli-version", ops: managerOps{
		inspect: func(context.Context, string) (procscope.Observation, error) {
			if stopped {
				return procscope.Observation{}, nil
			}
			return observation, nil
		},
		process: func(int) (procscope.ProcessIdentity, error) {
			if stopped {
				return procscope.ProcessIdentity{}, procscope.ErrProcessGone
			}
			return processToScope(state.Holder), nil
		},
		socket: func(*SessionStore) (managerSocket, error) {
			if stopped {
				return managerSocket{}, nil
			}
			return socket, nil
		},
		dial: func(context.Context, *SessionStore) (managerPeer, error) { return peer, nil },
		stop: func(_ context.Context, identity procscope.ScopeIdentity, birth procscope.ProcessIdentity) error {
			if identity != observation.Scope || birth != processToScope(state.Holder) {
				t.Fatal("quarantine cleanup targeted another invocation or birth")
			}
			stopCalls++
			stopped = true
			return nil
		},
		removeSocket: func(_ *SessionStore, expected managerSocket) error {
			if !stopped || expected != socket {
				t.Fatal("socket cleanup preceded exact scope stop or changed inode")
			}
			removed++
			return nil
		},
	}}
	status, err := m.Stop(context.Background(), "fixture")
	if err != nil || stopCalls != 1 || removed != 1 || !peer.closed || status.State.Guarantee != "forced_cleanup_no_flush" || status.State.Phase != "stopped" {
		t.Fatalf("quarantine stop did not complete honestly: status=%+v err=%v calls=%d removed=%d", status, err, stopCalls, removed)
	}
}

func TestSystemdRunMetadataRefusesCallerWritableOrNonSystemExecutable(t *testing.T) {
	if !trustedSystemdRun(0o100755, 0) {
		t.Fatal("root-owned fixed executable metadata refused")
	}
	for _, value := range []struct{ mode, uid uint32 }{
		{0o100775, 0}, {0o100757, 0}, {0o100755, 1000}, {0o100644, 0}, {0o040755, 0},
	} {
		if trustedSystemdRun(value.mode, value.uid) {
			t.Fatal("untrusted systemd-run metadata accepted")
		}
	}
}

func TestManagerDialKeepsCompatibleOldHolderAcrossCLIUpgrade(t *testing.T) {
	for _, incompatible := range []bool{false, true} {
		store, session := fixtureStore(t)
		writeManagerDeclarationFixture(t, store)
		snapshot, err := store.LoadConfig()
		if err != nil {
			t.Fatal(err)
		}
		state, hello, observation := managerRecordFixture()
		state.DeclarationRevision = snapshot.Sessions[0].Revision
		state.BinaryVersion = "old-holder-release"
		hello.DeclarationRevision, hello.BinaryVersion = state.DeclarationRevision, state.BinaryVersion
		if incompatible {
			state.ProtocolVersion++
			hello.ProtocolVersion = state.ProtocolVersion
		}
		if err := session.WriteState(state); err != nil {
			t.Fatal(err)
		}
		// A deterministic in-memory wire reply exercises the real Client.Status
		// request. The only injected boundary is the socket/hello establishment.
		encoded := &memoryConnection{input: bytes.NewReader(nil)}
		reply := frame("status")
		reply.State = &state
		if err := (&framedConn{conn: encoded}).write(context.Background(), reply); err != nil {
			t.Fatal(err)
		}
		connection := &memoryConnection{input: bytes.NewReader(append([]byte(nil), encoded.output.Bytes()...))}
		client := &Client{framed: &framedConn{conn: connection}, hello: hello}
		dials := 0
		m := &Manager{store: store, version: "newer-cli-release", ops: managerOps{
			attachDial: func(context.Context, *SessionStore) (*Client, error) {
				dials++
				return client, nil
			},
			inspect: func(context.Context, string) (procscope.Observation, error) { return observation, nil },
		}}
		got, err := m.Dial(context.Background(), "fixture")
		if dials != 1 {
			t.Fatal("attachment did not validate the same single connection")
		}
		if incompatible {
			if !errors.Is(err, ErrManager) || got != nil || !client.closed {
				t.Fatal("incompatible protocol was attached or left open")
			}
		} else {
			if err != nil || got != client || client.closed {
				t.Fatal("CLI upgrade made a protocol-compatible old holder unattachable", err)
			}
			_ = got.Close()
		}
	}
}
