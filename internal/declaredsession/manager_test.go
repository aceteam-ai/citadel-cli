package declaredsession

import (
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/procscope"
)

func managerRecordFixture() (State, Hello, procscope.Observation) {
	state := State{
		Version:             1,
		Name:                "fixture",
		Incarnation:         strings.Repeat("a", 32),
		Holder:              ProcessIdentity{PID: 42, StartTicks: 99, UID: 1000, BootID: "boot"},
		Scope:               ScopeIdentity{Name: "citadel-sess-fixture.scope", Manager: "user", InvocationID: strings.Repeat("1", 32), ControlGroup: "/user.slice/citadel-sess-fixture.scope"},
		ProtocolVersion:     ProtocolVersion,
		BinaryVersion:       "test-version",
		DeclarationRevision: digest([]byte("declaration")),
		Phase:               "running",
		Guarantee:           "verified_user_scope",
	}
	hello := Hello{ProtocolVersion: ProtocolVersion, BinaryVersion: state.BinaryVersion, Name: state.Name, Incarnation: state.Incarnation, Holder: state.Holder, Scope: state.Scope, DeclarationRevision: state.DeclarationRevision, Phase: state.Phase}
	observation := procscope.Observation{Scope: scopeFromState(state), Active: true, Members: []procscope.ProcessIdentity{processToScope(state.Holder)}}
	return state, hello, observation
}

func TestManagerRequiresSocketRecordAndLiveScopeIdentityTogether(t *testing.T) {
	stored, hello, observation := managerRecordFixture()
	if !sameHolderRecord(stored, hello, stored, observation) {
		t.Fatal("exact live identity refused")
	}
	for _, mutate := range []func(*State, *Hello, *procscope.Observation){
		func(s *State, h *Hello, o *procscope.Observation) {
			h.Incarnation = strings.Repeat("b", 32)
		},
		func(s *State, h *Hello, o *procscope.Observation) {
			h.Holder.StartTicks++
		},
		func(s *State, h *Hello, o *procscope.Observation) {
			h.Scope.InvocationID = strings.Repeat("2", 32)
		},
		func(s *State, h *Hello, o *procscope.Observation) {
			o.Scope.InvocationID = strings.Repeat("2", 32)
		},
		func(s *State, h *Hello, o *procscope.Observation) {
			o.Members = nil
		},
		func(s *State, h *Hello, o *procscope.Observation) {
			s.Holder.UID = 0
		},
		func(s *State, h *Hello, o *procscope.Observation) {
			h.ProtocolVersion++
		},
		func(s *State, h *Hello, o *procscope.Observation) {
			s.ProtocolVersion++
		},
		func(s *State, h *Hello, o *procscope.Observation) {
			h.BinaryVersion = "different-holder"
		},
		func(s *State, h *Hello, o *procscope.Observation) {
			s.BinaryVersion = "different-holder"
		},
	} {
		live, peer, observed := managerRecordFixture()
		mutate(&live, &peer, &observed)
		if sameHolderRecord(stored, peer, live, observed) {
			t.Fatal("PID file, stale connection or replaced scope supplied authority")
		}
	}
}

func TestAttachmentCompatibilityUsesProtocolNotCLIBinaryVersion(t *testing.T) {
	stored, hello, observation := managerRecordFixture()
	declaration := Declaration{Name: stored.Name, Revision: stored.DeclarationRevision}
	// A CLI update does not change the authenticated old holder's identity.
	// The holder's release is informational while its protocol remains valid.
	for _, holderVersion := range []string{"old-release", "current-release", "new-release"} {
		live := stored
		live.BinaryVersion, hello.BinaryVersion = holderVersion, holderVersion
		saved := live
		if !attachmentMatches(declaration, saved, hello, live, observation) {
			t.Fatal("compatible old or new holder release was refused")
		}
		live.ProtocolVersion++
		if attachmentMatches(declaration, saved, hello, live, observation) {
			t.Fatal("an incompatible holder protocol was accepted")
		}
	}
}

func TestDeclaredScopeNameRetainsExactLeaseName(t *testing.T) {
	for _, name := range []string{"a", "a.b_c-d", strings.Repeat("a", 64)} {
		if scope, err := ScopeName(name); err != nil || scope != "citadel-sess-"+name+".scope" {
			t.Fatal("canonical lease name changed")
		}
	}
	for _, name := range []string{"A", ".", "a/b", "a b", "a@b"} {
		if _, err := ScopeName(name); err == nil {
			t.Fatal("invalid name accepted")
		}
	}
}
