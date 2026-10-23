//go:build linux

package declaredsession

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/procscope"
)

func holderlessRecoveryFixture(t *testing.T) (*Manager, *SessionStore, State) {
	t.Helper()
	store, session := fixtureStore(t)
	writeManagerDeclarationFixture(t, store)
	snapshot, err := store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	state := State{
		Version:             1,
		Name:                "fixture",
		Incarnation:         strings.Repeat("a", 32),
		Scope:               ScopeIdentity{Name: "citadel-sess-fixture.scope", Manager: "user"},
		ProtocolVersion:     ProtocolVersion,
		BinaryVersion:       "original-launcher-version",
		StartedAt:           now,
		UpdatedAt:           now,
		FileRevision:        snapshot.FileRevision,
		DeclarationRevision: snapshot.Sessions[0].Revision,
		Phase:               "starting",
		Guarantee:           "unconfirmed",
	}
	if err := session.WriteState(state); err != nil {
		t.Fatal(err)
	}
	m := &Manager{store: store, executable: "/fixture/citadel", version: "new-cli-version", ops: managerOps{
		inspect: func(_ context.Context, name string) (procscope.Observation, error) {
			if name != state.Scope.Name {
				t.Fatal("recovery inspected a retargeted scope")
			}
			return procscope.Observation{}, nil
		},
		socket: func(*SessionStore) (managerSocket, error) { return managerSocket{}, nil },
		process: func(int) (procscope.ProcessIdentity, error) {
			t.Fatal("holderless recovery guessed a process birth")
			return procscope.ProcessIdentity{}, procscope.ErrUnknown
		},
		dial: func(context.Context, *SessionStore) (managerPeer, error) {
			t.Fatal("holderless recovery attempted a holder RPC")
			return nil, ErrManager
		},
		stop: func(context.Context, procscope.ScopeIdentity, procscope.ProcessIdentity) error {
			t.Fatal("holderless recovery signalled a scope or PID")
			return ErrManager
		},
		launch: func(context.Context, string, string, string, string, string, bool) (int, error) {
			t.Fatal("holderless recovery implicitly launched a replacement")
			return 0, ErrManager
		},
	}}
	return m, session, state
}

func TestManagerExplicitStopRetiresHolderlessStartingBeforeExplicitStart(t *testing.T) {
	m, session, pending := holderlessRecoveryFixture(t)
	if status, err := m.Start(context.Background(), pending.Name); !errors.Is(err, ErrManager) || status.Classification != "starting" {
		t.Fatal("Start implicitly retired a missing holder", status, err)
	}
	if _, err := m.Restart(context.Background(), pending.Name); !errors.Is(err, ErrManager) {
		t.Fatal("Restart implicitly recovered the empty reservation", err)
	}
	if _, exists, err := session.ReadStopMarker(); err != nil || exists {
		t.Fatal("refused implicit recovery wrote a stop marker", err)
	}
	stopped, err := m.Stop(context.Background(), pending.Name)
	if err != nil || stopped.Classification != "stopped" || stopped.State.Phase != "stopped" || stopped.State.Guarantee != "forced_cleanup_no_flush" || stopped.State.Holder != (ProcessIdentity{}) || stopped.State.Child != (ProcessIdentity{}) {
		t.Fatal("explicit recovery did not retire without a flush claim", stopped, err)
	}
	marker, present, err := session.ReadStopMarker()
	if err != nil || !present || marker.Name != pending.Name || marker.Incarnation != pending.Incarnation || marker.DeclarationRevision != pending.DeclarationRevision {
		t.Fatal("explicit recovery lost its correlated same-name veto", marker, err)
	}
	launches := 0
	m.ops.launch = func(context.Context, string, string, string, string, string, bool) (int, error) {
		launches++
		return 0, ErrManager // A fake pre-exec failure proves Start reaches launch.
	}
	if status, err := m.Start(context.Background(), pending.Name); !errors.Is(err, ErrManager) || status.Classification != "isolation_unavailable" || launches != 1 {
		t.Fatal("explicit Start could not proceed after completed recovery", status, err, launches)
	}
	if _, present, err := session.ReadStopMarker(); err != nil || present {
		t.Fatal("explicit Start did not clear only the proven old veto", err)
	}
}

func TestManagerHolderlessRecoveryFencesLateHolderBeforeProof(t *testing.T) {
	m, session, pending := holderlessRecoveryFixture(t)
	inspections := 0
	m.ops.inspect = func(_ context.Context, name string) (procscope.Observation, error) {
		inspections++
		if name != pending.Scope.Name {
			t.Fatal("proof inspected another scope")
		}
		if inspections == 2 {
			late, err := session.AcquireOwner(context.Background())
			if err == nil {
				late.Close()
				t.Fatal("late holder acquired ownership during empty-scope proof")
			}
		}
		return procscope.Observation{}, nil
	}
	if _, err := m.Stop(context.Background(), pending.Name); err != nil || inspections < 2 {
		t.Fatal("owner-fenced proof did not complete", err, inspections)
	}
	late, err := session.AcquireOwner(context.Background())
	if err != nil {
		t.Fatal("recovery retained the owner lock after durable retirement", err)
	}
	defer late.Close()
	current, present, err := session.ReadState()
	if err != nil || !present {
		t.Fatal(err)
	}
	_, declaration, err := m.declaration(pending.Name)
	if err != nil {
		t.Fatal(err)
	}
	options := RunHolderOptions{Name: pending.Name, ExpectedIncarnation: pending.Incarnation, ExpectedUnit: pending.Scope.Name, BinaryVersion: pending.BinaryVersion}
	if pendingLaunchMatches(current, options, declaration, ProcessIdentity{PID: 42, StartTicks: 1, UID: 1000, BootID: "boot"}) {
		t.Fatal("delayed holder recognized the retired reservation")
	}
	if _, present, err := session.ReadStopMarker(); err != nil || !present {
		t.Fatal("late holder lost the durable name veto", err)
	}
}

func TestManagerHolderlessRecoveryRefusesIncompleteOrUnknownProof(t *testing.T) {
	for _, test := range []struct {
		name  string
		proof procscope.Observation
		err   error
	}{
		{name: "scope present", proof: procscope.Observation{Scope: procscope.ScopeIdentity{Name: "citadel-sess-fixture.scope"}}},
		{name: "members present", proof: procscope.Observation{Members: []procscope.ProcessIdentity{{PID: 42}}}},
		{name: "partial scope identity", proof: procscope.Observation{Scope: procscope.ScopeIdentity{InvocationID: "unknown"}}},
		{name: "inspection error", err: procscope.ErrUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, session, pending := holderlessRecoveryFixture(t)
			m.ops.inspect = func(context.Context, string) (procscope.Observation, error) { return test.proof, test.err }
			if err := m.retireHolderlessStarting(context.Background(), session, pending); !errors.Is(err, ErrManager) {
				t.Fatal("uncertain proof permitted retirement", err)
			}
			current, present, err := session.ReadState()
			if err != nil || !present || !sameEmptyStartingReservation(current, pending) {
				t.Fatal("refused proof changed the reservation", err)
			}
			if _, present, err := session.ReadStopMarker(); err != nil || present {
				t.Fatal("refused proof wrote a marker", err)
			}
		})
	}
}

func TestManagerHolderlessRecoveryRequiresUnchangedEmptyReservation(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*State)
	}{
		{"holder birth", func(s *State) { s.Holder = ProcessIdentity{PID: 42, StartTicks: 1, UID: 1000, BootID: "boot"} }},
		{"partial holder", func(s *State) { s.Holder.StartTicks = 1 }},
		{"child birth", func(s *State) { s.Child.PID = 43 }},
		{"phase", func(s *State) { s.Phase = "running" }},
		{"incarnation", func(s *State) { s.Incarnation = strings.Repeat("b", 32) }},
		{"declaration revision", func(s *State) { s.DeclarationRevision = digest([]byte("changed")) }},
		{"file revision", func(s *State) { s.FileRevision = digest([]byte("changed")) }},
		{"protocol", func(s *State) { s.ProtocolVersion++ }},
		{"binary", func(s *State) { s.BinaryVersion = "other" }},
		{"scope invocation", func(s *State) { s.Scope.InvocationID = strings.Repeat("1", 32) }},
		{"scope cgroup", func(s *State) { s.Scope.ControlGroup = "/unproven" }},
		{"scope manager", func(s *State) { s.Scope.Manager = "system" }},
		{"guarantee", func(s *State) { s.Guarantee = "verified_user_scope" }},
		{"child generation", func(s *State) { s.ChildGeneration = 1 }},
		{"restart count", func(s *State) { s.RestartCount = 1 }},
		{"started timestamp", func(s *State) { s.StartedAt = s.StartedAt.Add(time.Second) }},
		{"updated timestamp", func(s *State) { s.UpdatedAt = s.UpdatedAt.Add(time.Second) }},
		{"resolved executable", func(s *State) { s.ResolvedExecutable = "/fixture/harness" }},
		{"environment", func(s *State) { s.EnvironmentHash = digest([]byte("environment")) }},
		{"last exit", func(s *State) { s.LastExit = &Exit{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, session, pending := holderlessRecoveryFixture(t)
			changed := pending
			test.edit(&changed)
			proofReached := false
			m.ops.inspect = func(context.Context, string) (procscope.Observation, error) {
				proofReached = true
				if err := session.WriteState(changed); err != nil {
					t.Fatal(err)
				}
				return procscope.Observation{}, nil
			}
			if err := m.retireHolderlessStarting(context.Background(), session, pending); !errors.Is(err, ErrManager) || !proofReached {
				t.Fatal("changed state was retired or race fixture did not reach proof", err)
			}
			current, present, err := session.ReadState()
			if err != nil || !present || current.Phase != changed.Phase || current.Guarantee != changed.Guarantee || !current.UpdatedAt.Equal(changed.UpdatedAt) {
				t.Fatal("recovery overwrote a changed reservation", err)
			}
			if _, present, err := session.ReadStopMarker(); err != nil || present {
				t.Fatal("changed reservation acquired a stop veto", err)
			}
		})
	}
}

func TestManagerHolderlessRecoveryRejectsHeldGuardsSocketAndCancellation(t *testing.T) {
	for _, kind := range []string{"owner", "operation", "socket", "socket error", "cancelled", "cancel during proof"} {
		t.Run(kind, func(t *testing.T) {
			m, session, pending := holderlessRecoveryFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "owner" {
				guard, err := session.AcquireOwner(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer guard.Close()
			}
			if kind == "operation" {
				guard, err := session.AcquireOperation(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer guard.Close()
			}
			if kind == "socket" || kind == "socket error" {
				m.ops.socket = func(*SessionStore) (managerSocket, error) {
					if kind == "socket error" {
						return managerSocket{}, ErrManager
					}
					return managerSocket{Present: true}, nil
				}
				m.ops.dial = func(context.Context, *SessionStore) (managerPeer, error) { return nil, ErrManager }
			}
			if kind == "cancelled" {
				cancel()
			}
			if kind == "cancel during proof" {
				inspections := 0
				m.ops.inspect = func(context.Context, string) (procscope.Observation, error) {
					inspections++
					if inspections == 2 {
						cancel()
					}
					return procscope.Observation{}, nil
				}
			}
			if _, err := m.Stop(ctx, pending.Name); err == nil {
				t.Fatal("held owner/operator, unexplained socket or cancellation allowed retirement")
			}
			current, _, err := session.ReadState()
			if err != nil || !sameEmptyStartingReservation(current, pending) {
				t.Fatal("refused recovery changed state", err)
			}
			if _, present, err := session.ReadStopMarker(); err != nil || present {
				t.Fatal("refused recovery wrote a marker", err)
			}
		})
	}
}

func TestManagerHolderlessRecoveryRetriesDurableMarkerBeforeStateCommit(t *testing.T) {
	m, session, pending := holderlessRecoveryFixture(t)
	marker := StopMarker{Version: 1, Name: pending.Name, DeclarationRevision: pending.DeclarationRevision, Incarnation: pending.Incarnation, StoppedAt: pending.UpdatedAt}
	if err := session.WriteStopMarker(marker); err != nil {
		t.Fatal(err)
	}
	// This is the durable intermediate state if the manager dies after marker
	// publication but before replacing starting. Start/Restart cannot clear it.
	if status, err := m.Status(context.Background(), pending.Name); err != nil || status.Classification != "recovery_incomplete" || !status.StopMarker {
		t.Fatal("durable partial recovery was reported as completed", status, err)
	}
	if status, err := m.Start(context.Background(), pending.Name); !errors.Is(err, ErrManager) || status.Classification != "recovery_incomplete" {
		t.Fatal("Start cleared a marker before retiring its starting reservation", err)
	}
	if _, err := m.Restart(context.Background(), pending.Name); !errors.Is(err, ErrManager) {
		t.Fatal("Restart implicitly completed a partial recovery", err)
	}
	if _, err := m.Stop(context.Background(), pending.Name); err != nil {
		t.Fatal("explicit Stop could not resume after partial durable publication", err)
	}
	retained, present, err := session.ReadStopMarker()
	if err != nil || !present || !retained.StoppedAt.Equal(marker.StoppedAt) || retained.Incarnation != marker.Incarnation {
		t.Fatal("retry rewrote or lost the original durable veto", retained, err)
	}
}

func TestHolderlessStartingRecoverySupportsOnlyKnownProtocolRecordShapes(t *testing.T) {
	for _, version := range []int{1, ProtocolVersion, 0, ProtocolVersion + 1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			m, session, pending := holderlessRecoveryFixture(t)
			pending.ProtocolVersion = version
			if err := session.WriteState(pending); err != nil {
				t.Fatal(err)
			}
			status, err := m.Stop(context.Background(), pending.Name)
			if version == 1 || version == ProtocolVersion {
				if err != nil || status.Classification != "stopped" || status.State.ProtocolVersion != version {
					t.Fatal("known holderless record could not be retired without rewriting its protocol", status, err)
				}
			} else {
				if !errors.Is(err, ErrManager) {
					t.Fatal("unknown holderless record protocol was retired", status, err)
				}
				if _, present, markerErr := session.ReadStopMarker(); markerErr != nil || present {
					t.Fatal("unknown protocol received a stop veto", markerErr)
				}
			}
		})
	}
}

func TestManagerHolderlessRecoveryRetainsConflictingNameVeto(t *testing.T) {
	m, session, pending := holderlessRecoveryFixture(t)
	marker := StopMarker{Version: 1, Name: pending.Name, DeclarationRevision: pending.DeclarationRevision, Incarnation: strings.Repeat("b", 32), StoppedAt: pending.UpdatedAt}
	if err := session.WriteStopMarker(marker); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stop(context.Background(), pending.Name); !errors.Is(err, ErrManager) {
		t.Fatal("conflicting durable marker became recovery authority", err)
	}
	current, _, err := session.ReadState()
	if err != nil || !sameEmptyStartingReservation(current, pending) {
		t.Fatal("conflicting marker caused retirement", err)
	}
	retained, present, err := session.ReadStopMarker()
	if err != nil || !present || retained.Incarnation != marker.Incarnation {
		t.Fatal("recovery overwrote another incarnation's name veto", err)
	}
}
