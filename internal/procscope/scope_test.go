package procscope

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type fakeScopeBackend struct {
	identity  ScopeIdentity
	pinErr    error
	checkErr  error
	stopErr   error
	stopCalls int
	closed    int
}

func (b *fakeScopeBackend) ByName(context.Context, string) (ScopeIdentity, bool, error) {
	return b.identity, true, nil
}
func (b *fakeScopeBackend) Pin(context.Context, ScopeIdentity) error {
	return b.pinErr
}
func (b *fakeScopeBackend) Check(context.Context, ScopeIdentity) error {
	return b.checkErr
}
func (b *fakeScopeBackend) Stop(context.Context, ScopeIdentity) error {
	b.stopCalls++
	return b.stopErr
}
func (b *fakeScopeBackend) Close() error {
	b.closed++
	return nil
}

func scopeFixture() (scopeOps, *fakeScopeBackend, ProcessIdentity) {
	process := ProcessIdentity{PID: 42, StartTicks: 999, UID: 1000, BootID: "a-boot"}
	backend := &fakeScopeBackend{identity: ScopeIdentity{Name: "citadel-sess-agent.scope", InvocationID: "11111111111111111111111111111111", ControlGroup: "/user.slice/user-1000.slice/user@1000.service/citadel-sess-agent.scope", UID: 1000}}
	ops := scopeOps{
		uid: func() uint32 {
			return 1000
		},
		self: func() int {
			return 42
		},
		process: func(int) (ProcessIdentity, string, error) {
			return process, backend.identity.ControlGroup, nil
		},
		members: func(context.Context, string) ([]ProcessIdentity, error) {
			return []ProcessIdentity{process}, nil
		},
		connect: func(context.Context, uint32) (scopeBackend, error) {
			return backend, nil
		},
	}
	return ops, backend, process
}

func TestStrictScopeLaunchIsLiteralAndNeverLegacyFallback(t *testing.T) {
	spec, err := NewUserScope("citadel-sess-a.b_c-d.scope", 1000, "/user.slice/worker.service")
	if err != nil {
		t.Fatal(err)
	}
	input := []string{"session-host", "a.b_c-d", "--node-config-dir=/tmp/a $literal path"}
	plan, err := spec.Launch("/usr/bin/systemd-run", "/usr/bin/citadel", input)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--user", "--scope", "--quiet", "--collect", "--no-ask-password", "--expand-environment=no", "--unit=citadel-sess-a.b_c-d.scope", "--", "/usr/bin/citadel", "session-host", "a.b_c-d", "--node-config-dir=/tmp/a $literal path"}
	if plan.Binary != "/usr/bin/systemd-run" || !reflect.DeepEqual(plan.Args, want) {
		t.Fatalf("literal plan = %+v", plan)
	}
	input[0] = "mutated"
	if plan.Args[9] != "session-host" {
		t.Fatal("launch aliases caller argv")
	}
	for _, unit := range []string{"", "../x.scope", "a/b.scope", `a\b.scope`, "x@a.scope", "x*.scope", ".scope", "-a.scope", "a\n.scope"} {
		if _, err := NewUserScope(unit, 1000, ""); err == nil {
			t.Fatalf("unsafe unit accepted %q", unit)
		}
	}
	if _, err := NewUserScope("a.scope", 0, ""); err == nil {
		t.Fatal("root scope accepted")
	}
}

func TestVerifiedScopeRefusesWorkerDescendantAndIdentityChanges(t *testing.T) {
	ops, backend, _ := scopeFixture()
	spec := ScopeSpec{Unit: backend.identity.Name, UID: 1000, OriginControlGroup: "/user.slice/worker.service"}
	proof, err := verifySelf(context.Background(), spec, ops)
	if err != nil {
		t.Fatal(err)
	}
	if proof.Identity() != backend.identity {
		t.Fatal("wrong exact identity")
	}
	_ = proof.Close()
	for _, change := range []func(*scopeOps, *fakeScopeBackend){
		func(o *scopeOps, b *fakeScopeBackend) {
			b.identity.ControlGroup = spec.OriginControlGroup + "/" + spec.Unit
		},
		func(o *scopeOps, b *fakeScopeBackend) {
			o.uid = func() uint32 {
				return 0
			}
		},
		func(o *scopeOps, b *fakeScopeBackend) {
			b.pinErr = ErrUnknown
		},
		func(o *scopeOps, b *fakeScopeBackend) {
			b.checkErr = ErrUnknown
		},
		func(o *scopeOps, b *fakeScopeBackend) {
			calls := 0
			before := o.process
			o.process = func(pid int) (ProcessIdentity, string, error) {
				p, g, e := before(pid)
				calls++
				if calls > 1 {
					p.StartTicks++
				}
				return p, g, e
			}
		},
	} {
		candidate, fake, _ := scopeFixture()
		change(&candidate, fake)
		if proof, err := verifySelf(context.Background(), spec, candidate); err == nil || proof != nil {
			t.Fatal("unverified holder received proof")
		}
	}
}

func TestUnexpectedScopeMemberPreventsChildRespawn(t *testing.T) {
	ops, backend, self := scopeFixture()
	proof, err := verifySelf(context.Background(), ScopeSpec{Unit: backend.identity.Name, UID: 1000}, ops)
	if err != nil {
		t.Fatal(err)
	}
	defer proof.Close()
	if err := proof.CheckMembers(context.Background(), []ProcessIdentity{self}); err != nil {
		t.Fatal(err)
	}
	proof.ops.members = func(context.Context, string) ([]ProcessIdentity, error) {
		other := self
		other.PID++
		other.StartTicks++
		return []ProcessIdentity{self, other}, nil
	}
	if proof.CheckMembers(context.Background(), []ProcessIdentity{self}) == nil {
		t.Fatal("living escaped group member permitted respawn")
	}
	if (&VerifiedScope{}).CheckMembers(context.Background(), nil) == nil {
		t.Fatal("zero proof accepted")
	}
}

func TestStopRejectsEmptyInvocationBeforeAnyBusConnection(t *testing.T) {
	ops, backend, self := scopeFixture()
	ops.connect = func(context.Context, uint32) (scopeBackend, error) {
		t.Fatal("invalid invocation reached bus")
		return nil, ErrUnknown
	}
	for _, id := range []string{"", "0", "00000000000000000000000000000000", "ABCDEF11111111111111111111111111", "not-an-invocation"} {
		identity := backend.identity
		identity.InvocationID = id
		if stopExact(context.Background(), identity, self, ops) == nil {
			t.Fatal("invalid invocation stopped")
		}
	}
}

func TestStopNeedsActualEmptyScopeAndOldBirthGone(t *testing.T) {
	ops, backend, self := scopeFixture()
	ctx, cancel := context.WithCancel(context.Background())
	ops.members = func(context.Context, string) ([]ProcessIdentity, error) {
		cancel()
		return []ProcessIdentity{self}, nil
	}
	if stopExact(ctx, backend.identity, self, ops) == nil {
		t.Fatal("method/job success was treated as actual exit")
	}
	if backend.stopCalls != 1 {
		t.Fatal("exact stop was not attempted once")
	}
	ops, backend, self = scopeFixture()
	ops.members = func(context.Context, string) ([]ProcessIdentity, error) {
		return nil, nil
	}
	ops.process = func(int) (ProcessIdentity, string, error) {
		return ProcessIdentity{}, "", ErrProcessGone
	}
	if err := stopExact(context.Background(), backend.identity, self, ops); err != nil {
		t.Fatal(err)
	}
	if backend.stopCalls != 1 {
		t.Fatal("wrong stop count")
	}
}

func TestAlreadyGoneInvocationNeverFallsBackToNameStop(t *testing.T) {
	ops, backend, self := scopeFixture()
	backend.pinErr, backend.checkErr = ErrScopeGone, ErrScopeGone
	ops.members = func(context.Context, string) ([]ProcessIdentity, error) {
		return nil, nil
	}
	ops.process = func(int) (ProcessIdentity, string, error) {
		return ProcessIdentity{}, "", ErrProcessGone
	}
	if err := stopExact(context.Background(), backend.identity, self, ops); err != nil {
		t.Fatal(err)
	}
	if backend.stopCalls != 0 {
		t.Fatal("gone invocation triggered destructive retry")
	}
	ops.process = func(int) (ProcessIdentity, string, error) {
		return self, "", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(stopExact(ctx, backend.identity, self, ops), ErrUnknown) {
		t.Fatal("live old holder falsely gone")
	}
}
