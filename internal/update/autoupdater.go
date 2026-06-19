// internal/update/autoupdater.go
// Opt-in periodic self-update for the long-lived Citadel agent.
//
// The AutoUpdater runs as a background goroutine launched by `citadel work`
// when enabled. On each tick it checks GitHub Releases for a newer version,
// and if one is found it downloads + checksum-verifies the binary, waits for
// an idle moment (no in-flight jobs), atomically swaps the running binary, and
// restarts the process so the new node-side capabilities take effect.
//
// Design notes:
//   - Reuses the existing Client (CheckForUpdate / DownloadAndVerify) and
//     ApplyUpdate / Rollback machinery so the release-asset naming and checksum
//     contract is preserved.
//   - This is a separate opt-in from the notify-only State.AutoUpdate gate used
//     by root.go: auto-INSTALL must be explicitly enabled and defaults off.
//   - Fail-safe: any error is reported via the logger and never panics or kills
//     the agent. In-flight jobs are always drained before the swap.
package update

import (
	"context"
	"fmt"
	"time"
)

const (
	// DefaultAutoUpdateInterval is how often the auto-updater checks for a
	// newer release when no interval is configured.
	DefaultAutoUpdateInterval = 1 * time.Hour

	// MinAutoUpdateInterval is the floor for the configured interval. The
	// unauthenticated GitHub API allows ~60 requests/hour, so we refuse to
	// poll more often than this to stay well within that budget.
	MinAutoUpdateInterval = 5 * time.Minute
)

// ReleaseChecker abstracts the "is there a newer release?" lookup so the loop
// can be tested without hitting the network. *Client satisfies it.
type ReleaseChecker interface {
	// CheckForUpdate returns a non-nil Release if a strictly newer version is
	// available, or (nil, nil) if already up to date.
	CheckForUpdate() (*Release, error)
	// DownloadAndVerify downloads the release asset for the current platform
	// to destPath and verifies its checksum.
	DownloadAndVerify(release *Release, destPath string) error
}

// AutoUpdaterConfig configures an AutoUpdater.
type AutoUpdaterConfig struct {
	// Checker performs release lookups and downloads. Required.
	Checker ReleaseChecker

	// Interval between checks. Values below MinAutoUpdateInterval are clamped
	// up to the floor. Zero uses DefaultAutoUpdateInterval.
	Interval time.Duration

	// ActiveJobs reports the number of in-flight jobs. When it returns 0 the
	// updater considers the node idle and safe to restart. If nil, the node is
	// always considered idle (best-effort).
	ActiveJobs func() int

	// Drain, if set, is called once an update is downloaded and verified to
	// stop the runner from fetching new jobs while we wait for idle.
	Drain func()

	// IdlePollInterval is how often to re-check ActiveJobs while waiting for
	// the node to drain. Zero uses a sensible default (2s).
	IdlePollInterval time.Duration

	// IdleTimeout bounds how long to wait for in-flight jobs to finish before
	// giving up on this update attempt (the next tick will retry). Zero uses a
	// sensible default (10m). A long-running job should never block the agent
	// from making progress on real work.
	IdleTimeout time.Duration

	// Apply replaces the running binary with the verified pending binary.
	// Defaults to ApplyUpdate. Overridable for testing.
	Apply func(pendingPath string) error

	// Restart re-execs / restarts the process onto the new binary.
	// Defaults to RestartProcess. Overridable for testing.
	Restart func() error

	// PendingPath is where the downloaded binary is staged.
	// Defaults to GetPendingBinaryPath().
	PendingPath string

	// Log reports progress and errors. Required for visibility; if nil a no-op
	// logger is used.
	Log func(format string, args ...any)
}

// AutoUpdater periodically checks for and applies updates.
type AutoUpdater struct {
	cfg AutoUpdaterConfig
}

// NewAutoUpdater constructs an AutoUpdater, applying defaults and clamping the
// interval to the safe floor.
func NewAutoUpdater(cfg AutoUpdaterConfig) *AutoUpdater {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultAutoUpdateInterval
	}
	if cfg.Interval < MinAutoUpdateInterval {
		cfg.Interval = MinAutoUpdateInterval
	}
	if cfg.IdlePollInterval <= 0 {
		cfg.IdlePollInterval = 2 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 10 * time.Minute
	}
	if cfg.Apply == nil {
		cfg.Apply = ApplyUpdate
	}
	if cfg.Restart == nil {
		cfg.Restart = RestartProcess
	}
	if cfg.PendingPath == "" {
		cfg.PendingPath = GetPendingBinaryPath()
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &AutoUpdater{cfg: cfg}
}

// Run blocks until ctx is cancelled, checking for updates on the configured
// interval. It never returns an error: failures are logged and retried on the
// next tick so the agent keeps running regardless.
func (a *AutoUpdater) Run(ctx context.Context) {
	a.cfg.Log("auto-update: enabled (interval %s)", a.cfg.Interval)
	ticker := time.NewTicker(a.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// runOnce never panics; guard anyway so a bug here can never take
			// down the agent.
			func() {
				defer func() {
					if r := recover(); r != nil {
						a.cfg.Log("auto-update: recovered from panic: %v", r)
					}
				}()
				if restarted := a.runOnce(ctx); restarted {
					// RestartProcess only returns on failure; if it somehow
					// returns success we still keep the loop alive.
					a.cfg.Log("auto-update: restart requested")
				}
			}()
		}
	}
}

// runOnce performs a single check-download-apply-restart cycle.
// It returns true if a restart was attempted (the process is expected to be
// replaced and not return). On any error it logs and returns false so the next
// tick retries.
func (a *AutoUpdater) runOnce(ctx context.Context) (restarted bool) {
	release, err := a.cfg.Checker.CheckForUpdate()
	if err != nil {
		a.cfg.Log("auto-update: check failed: %v", err)
		return false
	}
	if release == nil {
		a.cfg.Log("auto-update: up to date")
		return false
	}

	a.cfg.Log("auto-update: new version available: %s, downloading...", release.TagName)

	// Download + verify while jobs may still be running — this is the slow part
	// and does not require an idle node.
	if err := a.cfg.Checker.DownloadAndVerify(release, a.cfg.PendingPath); err != nil {
		a.cfg.Log("auto-update: download/verify failed: %v", err)
		return false
	}
	a.cfg.Log("auto-update: downloaded and verified %s", release.TagName)

	// Stop fetching new jobs, then wait for in-flight jobs to finish. Draining
	// BEFORE observing idle closes the race where a new job is picked up
	// between the idle check and the binary swap.
	if a.cfg.Drain != nil {
		a.cfg.Drain()
	}

	if err := a.waitForIdle(ctx); err != nil {
		a.cfg.Log("auto-update: %v; deferring to next cycle", err)
		return false
	}

	// Apply the swap (atomic; validates the new binary and rolls back on
	// failure internally).
	if err := a.cfg.Apply(a.cfg.PendingPath); err != nil {
		a.cfg.Log("auto-update: apply failed (kept current version): %v", err)
		return false
	}
	a.cfg.Log("auto-update: applied %s, restarting...", release.TagName)

	// Record the update in persistent state for the new process / status cmd.
	if state, serr := LoadState(); serr == nil {
		RecordUpdate(state, state.CurrentVersion, release.TagName)
		state.AvailableUpdate = ""
		UpdateLastCheck(state)
		_ = SaveState(state)
	}

	if err := a.cfg.Restart(); err != nil {
		// If restart fails the new binary is already in place; the supervisor
		// (systemd Restart=always / Windows SCM) will pick it up on the next
		// natural restart. Log and keep running on the old image for now.
		a.cfg.Log("auto-update: restart failed: %v (new binary will load on next restart)", err)
		return false
	}
	return true
}

// waitForIdle blocks until ActiveJobs reports 0, ctx is cancelled, or the idle
// timeout elapses.
func (a *AutoUpdater) waitForIdle(ctx context.Context) error {
	if a.cfg.ActiveJobs == nil {
		return nil
	}
	deadline := time.Now().Add(a.cfg.IdleTimeout)
	ticker := time.NewTicker(a.cfg.IdlePollInterval)
	defer ticker.Stop()

	for {
		if a.cfg.ActiveJobs() == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %d in-flight job(s) to finish",
				a.cfg.IdleTimeout, a.cfg.ActiveJobs())
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled while draining in-flight jobs")
		case <-ticker.C:
		}
	}
}

// ParseInterval parses a human duration string (e.g. "1h", "30m") into a
// duration suitable for AutoUpdaterConfig.Interval. Empty input yields the
// default interval. Invalid input returns an error so misconfiguration is
// surfaced rather than silently ignored.
func ParseInterval(s string) (time.Duration, error) {
	if s == "" {
		return DefaultAutoUpdateInterval, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid auto-update interval %q: %w", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("auto-update interval must be positive, got %q", s)
	}
	return d, nil
}
