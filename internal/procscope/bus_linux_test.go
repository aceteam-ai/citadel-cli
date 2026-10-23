//go:build linux

package procscope

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
)

type fakeScopeTransport struct {
	t          *testing.T
	identity   ScopeIdentity
	owner      string
	ctx        context.Context
	cancel     context.CancelFunc
	signals    chan<- *dbus.Signal
	watching   bool
	subscribed bool
	stopCalls  int
	stopMode   string
	changed    bool
}

func fakeBus(t *testing.T) (*userScopeBus, *fakeScopeTransport) {
	_, backend, _ := scopeFixture()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	transport := &fakeScopeTransport{t: t, identity: backend.identity, owner: ":1.7", ctx: ctx, cancel: cancel}
	return &userScopeBus{transport: transport, owner: transport.owner, uid: 1000}, transport
}

func (f *fakeScopeTransport) Context() context.Context {
	return f.ctx
}
func (f *fakeScopeTransport) Connected() bool {
	return f.ctx.Err() == nil
}
func (f *fakeScopeTransport) Close() error {
	f.cancel()
	return nil
}
func (f *fakeScopeTransport) WatchJobs(_ context.Context, owner string, signals chan<- *dbus.Signal) (func(), error) {
	if owner != f.owner {
		return nil, ErrUnknown
	}
	f.watching, f.signals = true, signals
	return func() {
		f.watching = false
	}, nil
}

func (f *fakeScopeTransport) Call(ctx context.Context, destination string, object dbus.ObjectPath, method string, args ...any) *dbus.Call {
	if ctx.Err() != nil {
		return &dbus.Call{Err: ctx.Err()}
	}
	reply := func(value any) *dbus.Call {
		return &dbus.Call{Body: []any{value}}
	}
	if method == "org.freedesktop.DBus.GetNameOwner" {
		return reply(f.owner)
	}
	if destination != ":1.7" {
		f.t.Fatal("method did not pin manager unique owner")
	}
	if method == managerInterface+".GetUnitByInvocationID" {
		id := args[0].([]byte)
		if len(id) != 16 {
			f.t.Fatal("wrong invocation argument")
		}
		if f.changed {
			return &dbus.Call{Err: dbus.Error{Name: "org.freedesktop.systemd1.NoUnitForInvocationID"}}
		}
		return reply(invocationObjectPath(f.identity.InvocationID))
	}
	if method == "org.freedesktop.DBus.Properties.Get" {
		var value any
		switch args[1] {
		case "Id":
			value = f.identity.Name
		case "InvocationID":
			value, _ = invocationBytes(f.identity.InvocationID)
		case "ControlGroup":
			value = f.identity.ControlGroup
		case "ActiveState":
			value = "active"
		default:
			f.t.Fatal("unexpected property")
		}
		return reply(dbus.MakeVariant(value))
	}
	if method == unitInterface+".Ref" || method == unitInterface+".Unref" {
		return &dbus.Call{}
	}
	if method == managerInterface+".Subscribe" {
		if !f.watching {
			f.t.Fatal("Subscribe before signal registration")
		}
		f.subscribed = true
		return &dbus.Call{}
	}
	if method == unitInterface+".Stop" {
		f.stopCalls++
		if !f.watching || !f.subscribed || object != invocationObjectPath(f.identity.InvocationID) || args[0] != "fail" {
			f.t.Fatal("unsafe stop address/order")
		}
		job := dbus.ObjectPath("/org/freedesktop/systemd1/job/23")
		emit := func(path dbus.ObjectPath) {
			f.signals <- &dbus.Signal{Sender: ":1.7", Path: managerPath, Name: managerInterface + ".JobRemoved", Body: []any{uint32(23), path, f.identity.Name, "done"}}
		}
		emit("/org/freedesktop/systemd1/job/22")
		switch f.stopMode {
		case "reply_lost":
			return &dbus.Call{Err: errors.New("ambiguous")}
		case "manager_changed":
			f.owner = ":1.8"
			emit(job)
		case "wrong_job":
			f.cancel()
		default:
			emit(job) // Before reply: a late subscription would lose this.
		}
		return reply(job)
	}
	f.t.Fatalf("unexpected method %s", method)
	return &dbus.Call{Err: ErrUnknown}
}

func TestInvocationStopRegistersBeforeReplyAndMatchesExactJob(t *testing.T) {
	bus, transport := fakeBus(t)
	if err := bus.Pin(context.Background(), transport.identity); err != nil {
		t.Fatal(err)
	}
	if err := bus.Stop(context.Background(), transport.identity); err != nil {
		t.Fatal(err)
	}
	if transport.stopCalls != 1 {
		t.Fatal("stop was retried")
	}
}

func TestAmbiguousStopNeverRetriesOrAcceptsUnrelatedJob(t *testing.T) {
	for _, mode := range []string{"reply_lost", "manager_changed", "wrong_job"} {
		t.Run(mode, func(t *testing.T) {
			bus, transport := fakeBus(t)
			if bus.Pin(context.Background(), transport.identity) != nil {
				t.Fatal("pin failed")
			}
			transport.stopMode = mode
			if bus.Stop(context.Background(), transport.identity) == nil {
				t.Fatal("ambiguous stop succeeded")
			}
			if transport.stopCalls != 1 {
				t.Fatal("destructive retry")
			}
		})
	}
}

func TestRetainedRefDoesNotAuthorizeChangedInvocation(t *testing.T) {
	bus, transport := fakeBus(t)
	if bus.Pin(context.Background(), transport.identity) != nil {
		t.Fatal("pin failed")
	}
	transport.changed = true
	if !errors.Is(bus.Stop(context.Background(), transport.identity), ErrUnknown) {
		t.Fatal("old Ref treated as invocation CAS")
	}
	if transport.stopCalls != 0 {
		t.Fatal("replaced unit was stopped by stable name")
	}
}

func TestZeroInvocationNeverCallsTransport(t *testing.T) {
	bus := &userScopeBus{}
	for _, id := range []string{"", strings.Repeat("0", 32)} {
		if _, err := bus.invocationPath(context.Background(), ScopeIdentity{InvocationID: id}); err == nil {
			t.Fatal("zero invocation accepted")
		}
	}
}

func TestInvocationPathUsesCanonicalSystemdBusLabel(t *testing.T) {
	for _, fixture := range []struct {
		id   string
		path dbus.ObjectPath
	}{
		{"11111111111111111111111111111111", dbus.ObjectPath("/org/freedesktop/systemd1/unit/_31" + strings.Repeat("1", 31))},
		{"0123456789abcdef0123456789abcdef", "/org/freedesktop/systemd1/unit/_30123456789abcdef0123456789abcdef"},
		{"abcdef0123456789abcdef0123456789", "/org/freedesktop/systemd1/unit/abcdef0123456789abcdef0123456789"},
	} {
		if got := invocationObjectPath(fixture.id); got != fixture.path {
			t.Fatalf("canonical invocation path = %q, want %q", got, fixture.path)
		}
		bus, transport := fakeBus(t)
		transport.identity.InvocationID = fixture.id
		if _, err := bus.invocationPath(context.Background(), transport.identity); err != nil {
			t.Fatal(err)
		}
	}
}
