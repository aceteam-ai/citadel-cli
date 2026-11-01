//go:build linux

package procscope

import (
	"context"
	"encoding/hex"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

const (
	managerName      = "org.freedesktop.systemd1"
	managerPath      = dbus.ObjectPath("/org/freedesktop/systemd1")
	managerInterface = "org.freedesktop.systemd1.Manager"
	unitInterface    = "org.freedesktop.systemd1.Unit"
	invocationPrefix = "/org/freedesktop/systemd1/unit/"
)

type userScopeBus struct {
	transport scopeTransport
	owner     string
	uid       uint32
	pinned    *ScopeIdentity
}

// The production transport and inert test transport execute the same stop/
// invocation/property state machine; tests need no socket or bus daemon.
type scopeTransport interface {
	Call(context.Context, string, dbus.ObjectPath, string, ...any) *dbus.Call
	WatchJobs(context.Context, string, chan<- *dbus.Signal) (func(), error)
	Context() context.Context
	Connected() bool
	Close() error
}

type liveScopeTransport struct{ conn *dbus.Conn }

func (t liveScopeTransport) Call(ctx context.Context, destination string, object dbus.ObjectPath, method string, args ...any) *dbus.Call {
	return t.conn.Object(destination, object).CallWithContext(ctx, method, 0, args...)
}

func (t liveScopeTransport) WatchJobs(ctx context.Context, owner string, signals chan<- *dbus.Signal) (func(), error) {
	t.conn.Signal(signals)
	cleanup := func() { t.conn.RemoveSignal(signals) }
	if err := t.conn.AddMatchSignalContext(ctx, dbus.WithMatchSender(owner), dbus.WithMatchObjectPath(managerPath), dbus.WithMatchInterface(managerInterface), dbus.WithMatchMember("JobRemoved")); err != nil {
		cleanup()
		return nil, err
	}
	return cleanup, nil
}

func (t liveScopeTransport) Context() context.Context { return t.conn.Context() }
func (t liveScopeTransport) Connected() bool          { return t.conn.Connected() }
func (t liveScopeTransport) Close() error             { return t.conn.Close() }

func connectUserScopeBus(ctx context.Context, uid uint32) (scopeBackend, error) {
	if uid == 0 || uid != nativeUID() {
		return nil, ErrIsolation
	}
	ctx, cancel := bounded(ctx)
	defer cancel()
	// Do not accept ambient DBUS_SESSION_BUS_ADDRESS or another user's runtime
	// directory. The peer must be this user's local bus daemon.
	address := "/run/user/" + strconv.FormatUint(uint64(uid), 10) + "/bus"
	raw, err := (&net.Dialer{}).DialContext(ctx, "unix", address)
	if err != nil {
		return nil, ErrIsolation
	}
	connection, ok := raw.(*net.UnixConn)
	if !ok {
		_ = raw.Close()
		return nil, ErrIsolation
	}
	peerOK := false
	control, err := connection.SyscallConn()
	if err == nil {
		err = control.Control(func(fd uintptr) {
			peer, peerErr := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
			peerOK = peerErr == nil && peer.Uid == uid
		})
	}
	if err != nil || !peerOK {
		_ = raw.Close()
		return nil, ErrIsolation
	}
	// Auth and Hello do not have context-taking APIs in the pinned client.
	// A transport deadline bounds these handshakes; all subsequent calls use
	// CallWithContext and this same private connection for their entire life.
	deadline, _ := ctx.Deadline()
	if raw.SetDeadline(deadline) != nil {
		_ = raw.Close()
		return nil, ErrIsolation
	}
	conn, err := dbus.NewConn(raw)
	if err != nil {
		_ = raw.Close()
		return nil, ErrIsolation
	}
	if conn.Auth([]dbus.Auth{dbus.AuthExternal(strconv.FormatUint(uint64(uid), 10))}) != nil || conn.Hello() != nil || raw.SetDeadline(time.Time{}) != nil {
		_ = conn.Close()
		return nil, ErrIsolation
	}
	bus := &userScopeBus{transport: liveScopeTransport{conn: conn}, uid: uid}
	if conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, managerName).Store(&bus.owner) != nil || !strings.HasPrefix(bus.owner, ":") {
		_ = conn.Close()
		return nil, ErrIsolation
	}
	var managerUID uint32
	if conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetConnectionUnixUser", 0, bus.owner).Store(&managerUID) != nil || managerUID != uid {
		_ = conn.Close()
		return nil, ErrIsolation
	}
	return bus, nil
}

func (b *userScopeBus) continuity(ctx context.Context) error {
	if !b.transport.Connected() {
		return ErrUnknown
	}
	var owner string
	if b.transport.Call(ctx, "org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus.GetNameOwner", managerName).Store(&owner) != nil || owner != b.owner {
		return ErrUnknown
	}
	return nil
}

func (b *userScopeBus) call(ctx context.Context, object dbus.ObjectPath, method string, args ...any) *dbus.Call {
	return b.transport.Call(ctx, b.owner, object, method, args...)
}

func (b *userScopeBus) property(ctx context.Context, object dbus.ObjectPath, name string, target any) error {
	var value dbus.Variant
	if b.call(ctx, object, "org.freedesktop.DBus.Properties.Get", unitInterface, name).Store(&value) != nil || value.Store(target) != nil {
		return ErrUnknown
	}
	return nil
}

func scopeBusError(err error, name string) bool {
	var value dbus.Error
	if errors.As(err, &value) {
		return value.Name == name
	}
	var pointer *dbus.Error
	return errors.As(err, &pointer) && pointer.Name == name
}

func (b *userScopeBus) readIdentity(ctx context.Context, object dbus.ObjectPath) (ScopeIdentity, bool, error) {
	var name, group, active string
	var invocation []byte
	if b.property(ctx, object, "Id", &name) != nil || b.property(ctx, object, "InvocationID", &invocation) != nil || b.property(ctx, object, "ControlGroup", &group) != nil || b.property(ctx, object, "ActiveState", &active) != nil {
		return ScopeIdentity{}, false, ErrUnknown
	}
	encoded := hex.EncodeToString(invocation)
	if _, err := invocationBytes(encoded); err != nil || !validUnit(name) || group != "" && !validCgroup(group) {
		return ScopeIdentity{}, false, ErrUnknown
	}
	return ScopeIdentity{Name: name, InvocationID: encoded, ControlGroup: group, UID: b.uid}, active == "active" || active == "activating", nil
}

func (b *userScopeBus) invocationPath(ctx context.Context, identity ScopeIdentity) (dbus.ObjectPath, error) {
	bytes, err := invocationBytes(identity.InvocationID)
	if err != nil {
		return "", err // In particular, NEVER send empty/all-zero IDs.
	}
	if b.continuity(ctx) != nil {
		return "", ErrUnknown
	}
	var object dbus.ObjectPath
	err = b.call(ctx, managerPath, managerInterface+".GetUnitByInvocationID", bytes).Store(&object)
	if scopeBusError(err, "org.freedesktop.systemd1.NoUnitForInvocationID") {
		return "", ErrScopeGone
	}
	// Official systemd returns the invocation-addressed path. Reject stable
	// unit-name paths even if some substitute implementation returns one.
	if err != nil || object != invocationObjectPath(identity.InvocationID) {
		return "", ErrUnknown
	}
	return object, nil
}

// unit_dbus_path_from_name uses systemd's bus_label_escape, including escaping
// a leading digit. The validated invocation alphabet is lowercase hex only.
func invocationObjectPath(id string) dbus.ObjectPath {
	label := id
	if len(id) > 0 && id[0] >= '0' && id[0] <= '9' {
		label = "_3" + id[:1] + id[1:]
	}
	return dbus.ObjectPath(invocationPrefix + label)
}

func (b *userScopeBus) ByName(ctx context.Context, unit string) (ScopeIdentity, bool, error) {
	if b.continuity(ctx) != nil || !validUnit(unit) {
		return ScopeIdentity{}, false, ErrUnknown
	}
	var object dbus.ObjectPath
	err := b.call(ctx, managerPath, managerInterface+".GetUnit", unit).Store(&object)
	if scopeBusError(err, "org.freedesktop.systemd1.NoSuchUnit") {
		return ScopeIdentity{}, false, nil
	}
	if err != nil || !object.IsValid() {
		return ScopeIdentity{}, false, ErrUnknown
	}
	identity, active, err := b.readIdentity(ctx, object)
	if err != nil || identity.Name != unit || !validCgroup(identity.ControlGroup) {
		return ScopeIdentity{}, false, ErrUnknown
	}
	return identity, active, b.Check(ctx, identity)
}

func (b *userScopeBus) Pin(ctx context.Context, identity ScopeIdentity) error {
	if b.pinned != nil {
		if *b.pinned != identity {
			return ErrUnknown
		}
		return b.Check(ctx, identity)
	}
	object, err := b.invocationPath(ctx, identity)
	if err != nil {
		return err
	}
	if b.Check(ctx, identity) != nil {
		return ErrUnknown
	}
	if b.call(ctx, object, unitInterface+".Ref").Err != nil {
		return ErrUnknown
	}
	copyOf := identity
	b.pinned = &copyOf
	return b.Check(ctx, identity)
}

func (b *userScopeBus) Check(ctx context.Context, identity ScopeIdentity) error {
	object, err := b.invocationPath(ctx, identity)
	if err != nil {
		return err
	}
	observed, active, err := b.readIdentity(ctx, object)
	if err != nil || observed.Name != identity.Name || observed.UID != identity.UID || observed.InvocationID != identity.InvocationID || observed.ControlGroup != identity.ControlGroup && (active || observed.ControlGroup != "") {
		return ErrUnknown
	}
	return b.continuity(ctx)
}

func (b *userScopeBus) Stop(ctx context.Context, identity ScopeIdentity) error {
	if b.pinned == nil || *b.pinned != identity || b.Check(ctx, identity) != nil {
		return ErrUnknown
	}
	object, err := b.invocationPath(ctx, identity)
	if err != nil {
		return ErrUnknown
	}
	signals := make(chan *dbus.Signal, 64)
	cleanup, err := b.transport.WatchJobs(ctx, b.owner, signals)
	if err != nil {
		return ErrUnknown
	}
	defer cleanup()
	if b.call(ctx, managerPath, managerInterface+".Subscribe").Err != nil {
		return ErrUnknown
	}
	// Subscribed BEFORE Stop. A signal arriving before the method reply stays
	// queued, but only the returned exact job path can acknowledge this action.
	var job dbus.ObjectPath
	if b.call(ctx, object, unitInterface+".Stop", "fail").Store(&job) != nil || !strings.HasPrefix(string(job), "/org/freedesktop/systemd1/job/") || !job.IsValid() {
		return ErrUnknown // An ambiguous reply MUST NOT trigger a retry.
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ErrUnknown
		case <-b.transport.Context().Done():
			return ErrUnknown
		case <-ticker.C:
			if b.continuity(ctx) != nil {
				return ErrUnknown
			}
		case signal := <-signals:
			if signal == nil || signal.Sender != b.owner || signal.Path != managerPath || signal.Name != managerInterface+".JobRemoved" || len(signal.Body) != 4 {
				continue
			}
			path, pathOK := signal.Body[1].(dbus.ObjectPath)
			unit, unitOK := signal.Body[2].(string)
			result, resultOK := signal.Body[3].(string)
			if !pathOK || path != job {
				continue
			}
			if !unitOK || !resultOK || unit != identity.Name || result != "done" || b.continuity(ctx) != nil {
				return ErrUnknown
			}
			return nil // Caller separately proves empty cgroup + old birth gone.
		}
	}
}

func (b *userScopeBus) Close() error {
	if b == nil || b.transport == nil {
		return nil
	}
	if b.pinned != nil {
		ctx, cancel := bounded(context.Background())
		if object, err := b.invocationPath(ctx, *b.pinned); err == nil {
			_ = b.call(ctx, object, unitInterface+".Unref").Err
		}
		cancel()
	}
	// Closing this private connection also releases any ambiguous Ref and all
	// subscriptions. There is no reconnect or name-addressed destructive retry.
	return b.transport.Close()
}
