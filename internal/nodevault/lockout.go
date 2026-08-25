package nodevault

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Lockout tuning. A short numeric PIN is only safe to gate access AT ALL
// because it is rate-limited; without a persisted, cross-process counter,
// "lockout after N attempts" is decorative (docs §5). These are v1 constants;
// they can graduate to Policy fields later without changing the state format.
const (
	// maxFreeAttempts failures are allowed before any lockout window applies.
	maxFreeAttempts = 5
	// lockoutBase is the first lockout window; it doubles per additional
	// failure up to lockoutCap. Auto-wipe is intentionally OFF (issue #796):
	// the node hard-locks and waits, it never destroys the vault, so a
	// legitimate user who fat-fingers the PIN loses time, not data.
	lockoutBase = 30 * time.Second
	lockoutCap  = time.Hour
)

const lockoutStateFile = "state.yaml"

// ErrLockedOut is returned when the vault is in a lockout window. It carries
// the time the window ends so a surface can show a countdown.
type ErrLockedOut struct {
	Until time.Time
}

func (e *ErrLockedOut) Error() string {
	return fmt.Sprintf("nodevault: locked out after too many failed attempts; try again after %s",
		e.Until.Format(time.RFC3339))
}

// IsLockedOut reports whether err is an ErrLockedOut.
func IsLockedOut(err error) bool {
	var e *ErrLockedOut
	return errors.As(err, &e)
}

// lockoutState is the node-wide (not per-identity, docs §5 Resolved) attempt
// counter, persisted next to the vault so every process and every gate surface
// converges on one counter across restarts.
type lockoutState struct {
	FailedAttempts int       `yaml:"failed_attempts"`
	LockoutUntil   time.Time `yaml:"lockout_until"`
	LastVerifiedAt time.Time `yaml:"last_verified_at"`
}

func (v *Vault) lockoutPath() string { return filepath.Join(v.dir, lockoutStateFile) }

func (v *Vault) readLockout() lockoutState {
	var s lockoutState
	data, err := os.ReadFile(v.lockoutPath())
	if err != nil {
		return s // absent = clean slate
	}
	_ = yaml.Unmarshal(data, &s)
	return s
}

func (v *Vault) writeLockout(s lockoutState) error {
	data, err := yaml.Marshal(s)
	if err != nil {
		return fmt.Errorf("nodevault: marshal lockout state: %w", err)
	}
	return writeFileAtomic(v.lockoutPath(), data, 0o600)
}

// checkLockedLocked returns ErrLockedOut if a lockout window is active. Read
// fresh from disk so a lockout set by another process/surface is honored.
// Caller holds kdfMu (in-process serialization); the cross-process counter is
// best-effort node-wide (a benign race can under-count under simultaneous
// multi-process attack, still bounded by the KDF cost).
func (v *Vault) checkLockedLocked() error {
	s := v.readLockout()
	if !s.LockoutUntil.IsZero() && time.Now().Before(s.LockoutUntil) {
		return &ErrLockedOut{Until: s.LockoutUntil}
	}
	return nil
}

func (v *Vault) recordFailureLocked() {
	s := v.readLockout()
	s.FailedAttempts++
	if s.FailedAttempts >= maxFreeAttempts {
		s.LockoutUntil = time.Now().Add(backoff(s.FailedAttempts))
	}
	_ = v.writeLockout(s)
}

func (v *Vault) recordSuccessLocked() {
	// Reset the counter and refresh last-verified (the sliding re-prompt window
	// anchor, docs §5). A correct entry clears any accrued lockout.
	_ = v.writeLockout(lockoutState{LastVerifiedAt: time.Now()})
}

// backoff returns the lockout window for the given failure count: geometric
// growth from lockoutBase, capped at lockoutCap.
func backoff(failures int) time.Duration {
	over := failures - maxFreeAttempts
	if over < 0 {
		over = 0
	}
	d := lockoutBase
	for i := 0; i < over; i++ {
		d *= 2
		if d >= lockoutCap {
			return lockoutCap
		}
	}
	if d > lockoutCap {
		return lockoutCap
	}
	return d
}

// LockoutStatus reports the current attempt/lockout state for surfaces.
type LockoutStatus struct {
	FailedAttempts int
	LockedOut      bool
	LockoutUntil   time.Time
	LastVerifiedAt time.Time
}

// LockoutStatus returns the current node-wide lockout state.
func (v *Vault) LockoutStatus() LockoutStatus {
	s := v.readLockout()
	return LockoutStatus{
		FailedAttempts: s.FailedAttempts,
		LockedOut:      !s.LockoutUntil.IsZero() && time.Now().Before(s.LockoutUntil),
		LockoutUntil:   s.LockoutUntil,
		LastVerifiedAt: s.LastVerifiedAt,
	}
}

// ResetLockout clears the attempt counter and lockout window. It exists for a
// local-presence recovery path (a human at the machine), consistent with
// "hard lockout requiring local presence" rather than auto-wipe: someone with
// local access can clear the lock without waiting out the window and without
// any data loss.
func (v *Vault) ResetLockout() error {
	return v.writeLockout(lockoutState{LastVerifiedAt: v.readLockout().LastVerifiedAt})
}
