//go:build linux

package declaredsession

import (
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
