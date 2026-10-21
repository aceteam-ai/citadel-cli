package nodevault

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
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

	// Keep escalation across capped lockouts; correct PINs do not erase recent
	// guesses. At most the newest 64 timestamps are persisted (#1371).
	failureWindow     = 24 * time.Hour
	maxFailureHistory = 64
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

	// No omitempty: an explicit empty list distinguishes expired/reset history
	// from legacy state that has only an undated failed_attempts counter.
	FailureTimes []time.Time `yaml:"failure_times"`
}

// Lockout is the node-wide persisted attempt limiter over one state file. The
// master-PIN vault uses it for VerifyPIN/Unlock/ChangePIN, and the legacy bcrypt
// passcode gate uses it through Attempt (#1398), so a node's network passcode
// surfaces are rate-limited whether or not a master PIN is enrolled. A node is in
// exactly one mode (vault enrolled or legacy passcode), so both share ONE file,
// <baseDir>/vault/state.yaml: one counter per node, with the same thresholds,
// backoff, rolling window and history bound in either mode.
type Lockout struct {
	path string
}

// OpenLockout returns the node-wide limiter whose state lives under baseDir
// (in production, network.GetNodeConfigDir()). It is the same state a vault
// opened over baseDir uses. It does not touch disk.
func OpenLockout(baseDir string) *Lockout {
	return &Lockout{path: filepath.Join(baseDir, vaultDirName, lockoutStateFile)}
}

// lockoutMus serializes lockout read-modify-write per state file within this
// process. It is keyed by path, not held per handle, because gate callers open a
// fresh Vault/Lockout per check: a per-handle mutex would not serialize them.
// Cross-process writers remain best-effort (#1395).
var lockoutMus sync.Map // path -> *sync.Mutex

func lockoutMutex(path string) *sync.Mutex {
	mu, _ := lockoutMus.LoadOrStore(path, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// Attempt runs check under the node-wide lockout. It refuses with
// *ErrLockedOut, without running check, while a lockout window is active (even
// for a correct secret). A false check records one failure and returns
// ErrWrongPIN; a true check refreshes the verification anchor without clearing
// recent failures and returns nil. The per-path mutex is held across the check,
// as the vault holds kdfMu across its KDF, so concurrent guesses in one process
// cannot all pass the admission check before any failure is recorded.
//
// Callers reject an empty secret BEFORE calling Attempt: an empty secret is
// never counted, so unauthenticated probes cannot lock the node.
func (l *Lockout) Attempt(check func() bool) error {
	mu := lockoutMutex(l.path)
	mu.Lock()
	defer mu.Unlock()
	if err := checkLockedAt(l.path); err != nil {
		return err
	}
	if !check() {
		recordFailureAt(l.path)
		return ErrWrongPIN
	}
	recordSuccessAt(l.path)
	return nil
}

// Status returns the current node-wide lockout state.
func (l *Lockout) Status() LockoutStatus {
	now := time.Now()
	s := readLockoutAt(l.path, now)
	return LockoutStatus{
		FailedAttempts: s.FailedAttempts,
		LockedOut:      !s.LockoutUntil.IsZero() && now.Before(s.LockoutUntil),
		LockoutUntil:   s.LockoutUntil,
		LastVerifiedAt: s.LastVerifiedAt,
	}
}

// Reset clears the attempt history and lockout window, keeping the
// verification anchor. It is the local-presence recovery path.
func (l *Lockout) Reset() error {
	mu := lockoutMutex(l.path)
	mu.Lock()
	defer mu.Unlock()
	return writeLockoutAt(l.path, lockoutState{LastVerifiedAt: readLockoutAt(l.path, time.Now()).LastVerifiedAt, FailureTimes: []time.Time{}})
}

func (v *Vault) lockoutPath() string { return filepath.Join(v.dir, lockoutStateFile) }

// Lockout returns the node-wide limiter backing this vault.
func (v *Vault) Lockout() *Lockout { return &Lockout{path: v.lockoutPath()} }

func (v *Vault) readLockout() lockoutState {
	return readLockoutAt(v.lockoutPath(), time.Now())
}

func (v *Vault) readLockoutAt(now time.Time) lockoutState {
	return readLockoutAt(v.lockoutPath(), now)
}

func readLockoutAt(path string, now time.Time) lockoutState {
	var s lockoutState
	data, err := os.ReadFile(path)
	if err != nil {
		return normalizeLockout(s, now) // absent = clean slate
	}
	_ = yaml.Unmarshal(data, &s)
	return normalizeLockout(s, now)
}

// normalizeLockout is pure. Reading status never writes/migrates live state.
// The next existing failure/success write persists the migration once: legacy
// counts have unknown age, so conservatively seed them at that write's time.
// Keep the old scalar in sync for older binaries; this does not make mixed-
// version writers or concurrent processes safe (separate issue #1395).
func normalizeLockout(s lockoutState, now time.Time) lockoutState {
	history := make([]time.Time, 0)
	if s.FailureTimes == nil {
		for i := 0; i < min(max(s.FailedAttempts, 0), maxFailureHistory); i++ {
			history = append(history, now)
		}
	} else {
		cutoff := now.Add(-failureWindow)
		for _, failedAt := range s.FailureTimes {
			// Retain future timestamps on clock rollback, rather than dropping
			// recent failures and granting an unintended reset.
			if failedAt.After(cutoff) {
				history = append(history, failedAt)
			}
		}
	}
	sort.Slice(history, func(i, j int) bool { return history[i].Before(history[j]) })
	if len(history) > maxFailureHistory {
		history = history[len(history)-maxFailureHistory:]
	}
	s.FailureTimes = history
	s.FailedAttempts = len(history)
	return s
}

func (v *Vault) writeLockout(s lockoutState) error {
	return writeLockoutAt(v.lockoutPath(), s)
}

func writeLockoutAt(path string, s lockoutState) error {
	data, err := yaml.Marshal(s)
	if err != nil {
		return fmt.Errorf("nodevault: marshal lockout state: %w", err)
	}
	return writeFileAtomic(path, data, 0o600)
}

// checkLockedLocked returns ErrLockedOut if a lockout window is active. Read
// fresh from disk so a lockout set by another process/surface is honored.
// Caller holds kdfMu; the per-path lockout mutex is taken for the read. The
// cross-process counter is best-effort node-wide (a benign race can
// under-count under simultaneous multi-process attack, still bounded by the
// KDF cost).
func (v *Vault) checkLockedLocked() error {
	mu := lockoutMutex(v.lockoutPath())
	mu.Lock()
	defer mu.Unlock()
	return checkLockedAt(v.lockoutPath())
}

func (v *Vault) recordFailureLocked() {
	mu := lockoutMutex(v.lockoutPath())
	mu.Lock()
	defer mu.Unlock()
	recordFailureAt(v.lockoutPath())
}

func (v *Vault) recordSuccessLocked() {
	mu := lockoutMutex(v.lockoutPath())
	mu.Lock()
	defer mu.Unlock()
	recordSuccessAt(v.lockoutPath())
}

// checkLockedAt, recordFailureAt and recordSuccessAt are the lockout rules
// shared by every caller. The caller holds lockoutMutex(path).
func checkLockedAt(path string) error {
	s := readLockoutAt(path, time.Now())
	if !s.LockoutUntil.IsZero() && time.Now().Before(s.LockoutUntil) {
		return &ErrLockedOut{Until: s.LockoutUntil}
	}
	return nil
}

func recordFailureAt(path string) {
	now := time.Now()
	s := readLockoutAt(path, now)
	s.FailureTimes = append(s.FailureTimes, now)
	s = normalizeLockout(s, now)
	if s.FailedAttempts >= maxFreeAttempts {
		if until := now.Add(backoff(s.FailedAttempts)); until.After(s.LockoutUntil) {
			s.LockoutUntil = until
		}
	}
	_ = writeLockoutAt(path, s)
}

func recordSuccessAt(path string) {
	// Refresh the re-prompt anchor only. Recent guesses and any active lockout
	// survive successes on every PIN surface sharing this persisted state.
	now := time.Now()
	s := readLockoutAt(path, now)
	s.LastVerifiedAt = now
	_ = writeLockoutAt(path, s)
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
func (v *Vault) LockoutStatus() LockoutStatus { return v.Lockout().Status() }

// ResetLockout clears the attempt counter and lockout window. It exists for a
// local-presence recovery path (a human at the machine), consistent with
// "hard lockout requiring local presence" rather than auto-wipe: someone with
// local access can clear the lock without waiting out the window and without
// any data loss. The state is node-wide, so this also clears a lockout earned
// by legacy passcode checks (#1398).
func (v *Vault) ResetLockout() error { return v.Lockout().Reset() }
