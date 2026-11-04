// Package procscope verifies the independent user scope of a durable process.
// It deliberately does not share tmux's legacy fail-open launch policy.
package procscope

import (
	"context"
	"encoding/hex"
	"errors"
	"path"
	"strings"
	"time"
)

var (
	ErrIsolation   = errors.New("declared session isolation unavailable")
	ErrUnknown     = errors.New("declared session scope observation unconfirmed")
	ErrProcessGone = errors.New("declared session process no longer exists")
	ErrScopeGone   = errors.New("declared session scope invocation no longer exists")
)

const operationTimeout = 5 * time.Second

type ProcessIdentity struct {
	PID        int
	StartTicks uint64
	UID        uint32
	BootID     string
}

type ScopeIdentity struct {
	Name         string
	InvocationID string
	ControlGroup string
	UID          uint32
}

type ScopeSpec struct {
	Unit               string
	UID                uint32
	OriginControlGroup string
}

type Observation struct {
	Scope   ScopeIdentity
	Active  bool
	Members []ProcessIdentity
}

// Launch is only a direct argv plan, never proof that its scope exists.
type Launch struct {
	Binary string
	Args   []string
}

func NewUserScope(unit string, uid uint32, origin string) (ScopeSpec, error) {
	if !validUnit(unit) || uid == 0 || origin != "" && !validCgroup(origin) {
		return ScopeSpec{}, ErrIsolation
	}
	return ScopeSpec{Unit: unit, UID: uid, OriginControlGroup: origin}, nil
}

func (s ScopeSpec) Launch(systemdRun, binary string, args []string) (Launch, error) {
	if !validUnit(s.Unit) || s.UID == 0 || !strings.HasPrefix(systemdRun, "/") || !strings.HasPrefix(binary, "/") {
		return Launch{}, ErrIsolation
	}
	argv := []string{"--user", "--scope", "--quiet", "--collect", "--no-ask-password", "--expand-environment=no", "--unit=" + s.Unit, "--", binary}
	argv = append(argv, args...)
	return Launch{Binary: systemdRun, Args: argv}, nil
}

func validUnit(unit string) bool {
	if len(unit) > 255 || !strings.HasSuffix(unit, ".scope") || len(unit) <= len(".scope") {
		return false
	}
	for _, c := range unit {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return unit[0] != '.' && unit[0] != '-'
}

func validCgroup(value string) bool {
	return value != "/" && strings.HasPrefix(value, "/") && path.Clean(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func within(group, parent string) bool {
	return group == parent || strings.HasPrefix(group, parent+"/")
}

// Empty/all-zero invocation IDs address the CALLER's unit in systemd. They
// must be refused before even making GetUnitByInvocationID.
func invocationBytes(value string) ([]byte, error) {
	if len(value) != 32 || strings.ToLower(value) != value {
		return nil, ErrUnknown
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 16 {
		return nil, ErrUnknown
	}
	for _, b := range decoded {
		if b != 0 {
			return decoded, nil
		}
	}
	return nil, ErrUnknown
}

func bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, operationTimeout)
}

func sameProcess(a, b ProcessIdentity) bool {
	return a.PID > 0 && a.StartTicks != 0 && a.BootID != "" && a == b
}

type scopeBackend interface {
	ByName(context.Context, string) (ScopeIdentity, bool, error)
	Pin(context.Context, ScopeIdentity) error
	Check(context.Context, ScopeIdentity) error
	Stop(context.Context, ScopeIdentity) error
	Close() error
}

type scopeOps struct {
	uid     func() uint32
	self    func() int
	process func(int) (ProcessIdentity, string, error)
	members func(context.Context, string) ([]ProcessIdentity, error)
	connect func(context.Context, uint32) (scopeBackend, error)
}

// VerifiedScope is created only by live self verification. Ref pins collection,
// not invocation identity; every operation still uses the invocation path.
type VerifiedScope struct {
	identity ScopeIdentity
	self     ProcessIdentity
	backend  scopeBackend
	ops      scopeOps
}

func (v *VerifiedScope) Identity() ScopeIdentity { return v.identity }

func (v *VerifiedScope) Close() error {
	if v == nil || v.backend == nil {
		return nil
	}
	return v.backend.Close()
}

func VerifySelf(ctx context.Context, spec ScopeSpec) (*VerifiedScope, error) {
	return verifySelf(ctx, spec, nativeOps())
}

func verifySelf(ctx context.Context, spec ScopeSpec, ops scopeOps) (*VerifiedScope, error) {
	if !validUnit(spec.Unit) || spec.UID == 0 || ops.uid() != spec.UID || spec.OriginControlGroup != "" && !validCgroup(spec.OriginControlGroup) {
		return nil, ErrIsolation
	}
	ctx, cancel := bounded(ctx)
	defer cancel()
	self, group, err := ops.process(ops.self())
	if err != nil || self.UID != spec.UID || !validCgroup(group) || path.Base(group) != spec.Unit || spec.OriginControlGroup != "" && within(group, spec.OriginControlGroup) {
		return nil, ErrIsolation
	}
	backend, err := ops.connect(ctx, spec.UID)
	if err != nil {
		return nil, ErrIsolation
	}
	identity, active, err := backend.ByName(ctx, spec.Unit)
	if err != nil || !active || identity.UID != spec.UID || identity.ControlGroup != group || identity.Name != spec.Unit {
		_ = backend.Close()
		return nil, ErrIsolation
	}
	if err := backend.Pin(ctx, identity); err != nil {
		_ = backend.Close()
		return nil, ErrIsolation
	}
	after, afterGroup, err := ops.process(self.PID)
	if err != nil || !sameProcess(self, after) || group != afterGroup || backend.Check(ctx, identity) != nil {
		_ = backend.Close()
		return nil, ErrIsolation
	}
	return &VerifiedScope{identity: identity, self: self, backend: backend, ops: ops}, nil
}

// CheckMembers refuses automatic child respawn if any unexpected process is
// still in this exact scope, including a child which escaped its process group.
func (v *VerifiedScope) CheckMembers(ctx context.Context, allowed []ProcessIdentity) error {
	if v == nil || v.backend == nil {
		return ErrUnknown
	}
	ctx, cancel := bounded(ctx)
	defer cancel()
	if err := v.backend.Check(ctx, v.identity); err != nil {
		return ErrUnknown
	}
	self, group, err := v.ops.process(v.self.PID)
	if err != nil || !sameProcess(self, v.self) || group != v.identity.ControlGroup {
		return ErrUnknown
	}
	members, err := v.ops.members(ctx, group)
	if err != nil {
		return ErrUnknown
	}
	for _, member := range members {
		found := false
		for _, permitted := range allowed {
			if sameProcess(member, permitted) {
				found = true
				break
			}
		}
		if !found {
			return ErrUnknown
		}
	}
	return v.backend.Check(ctx, v.identity)
}

func Inspect(ctx context.Context, unit string) (Observation, error) {
	return inspect(ctx, unit, nativeOps())
}

func inspect(ctx context.Context, unit string, ops scopeOps) (Observation, error) {
	if !validUnit(unit) || ops.uid() == 0 {
		return Observation{}, ErrIsolation
	}
	ctx, cancel := bounded(ctx)
	defer cancel()
	backend, err := ops.connect(ctx, ops.uid())
	if err != nil {
		return Observation{}, ErrIsolation
	}
	defer backend.Close()
	identity, active, err := backend.ByName(ctx, unit)
	if err != nil {
		return Observation{}, err
	}
	if identity.Name == "" {
		return Observation{}, nil
	}
	if backend.Pin(ctx, identity) != nil {
		return Observation{}, ErrUnknown
	}
	members, err := ops.members(ctx, identity.ControlGroup)
	if err != nil || backend.Check(ctx, identity) != nil {
		return Observation{}, ErrUnknown
	}
	return Observation{Scope: identity, Active: active, Members: members}, nil
}

func StopExact(ctx context.Context, identity ScopeIdentity, oldHolder ProcessIdentity) error {
	return stopExact(ctx, identity, oldHolder, nativeOps())
}

func stopExact(ctx context.Context, identity ScopeIdentity, oldHolder ProcessIdentity, ops scopeOps) error {
	if _, err := invocationBytes(identity.InvocationID); err != nil {
		return err
	}
	if !validUnit(identity.Name) || !validCgroup(identity.ControlGroup) || identity.UID == 0 || identity.UID != ops.uid() || oldHolder.UID != identity.UID || oldHolder.PID <= 0 || oldHolder.StartTicks == 0 || oldHolder.BootID == "" {
		return ErrUnknown
	}
	ctx, cancel := bounded(ctx)
	defer cancel()
	backend, err := ops.connect(ctx, identity.UID)
	if err != nil {
		return ErrUnknown
	}
	defer backend.Close()
	if err := backend.Pin(ctx, identity); err != nil {
		if !errors.Is(err, ErrScopeGone) {
			return ErrUnknown
		}
		// A graceful holder may already have removed its old invocation. There
		// is no destructive call or job to acknowledge in this branch. Absence
		// still requires an empty old cgroup and the exact old birth gone.
	} else if backend.Stop(ctx, identity) != nil {
		return ErrUnknown
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := backend.Check(ctx, identity); err != nil && !errors.Is(err, ErrScopeGone) {
			return ErrUnknown
		}
		members, err := ops.members(ctx, identity.ControlGroup)
		if err != nil {
			return ErrUnknown
		}
		process, _, procErr := ops.process(oldHolder.PID)
		gone := errors.Is(procErr, ErrProcessGone) || procErr == nil && !sameProcess(process, oldHolder)
		if len(members) == 0 && gone {
			return nil
		}
		if procErr != nil && !errors.Is(procErr, ErrProcessGone) {
			return ErrUnknown
		}
		select {
		case <-ctx.Done():
			return ErrUnknown
		case <-ticker.C:
		}
	}
}
