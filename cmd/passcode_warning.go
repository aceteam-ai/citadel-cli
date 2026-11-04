package cmd

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
)

type passcodeWarningMode uint8

const (
	passcodeWarningMaster passcodeWarningMode = iota
	passcodeWarningLegacy
	passcodeWarningModeCount
)

type passcodeWarningSlot struct {
	nodeDir string
	last    time.Time
	seen    bool
}

// Keep only the current context per mode, not an unbounded per-path history.
// Paths are private equality keys and are never included in diagnostic output.
type passcodeWarningReminder struct {
	mu    sync.Mutex
	slots [passcodeWarningModeCount]passcodeWarningSlot
}

var passcodeWarnings passcodeWarningReminder

func (r *passcodeWarningReminder) shouldWarn(mode passcodeWarningMode, nodeDir string, clock func() time.Time) bool {
	if mode >= passcodeWarningModeCount {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Read time under the mutex so reordered callers do not look like a
	// backward clock and each trigger a new warning.
	now := clock()
	slot := &r.slots[mode]
	if slot.seen && slot.nodeDir == nodeDir && !now.Before(slot.last) && now.Sub(slot.last) < time.Minute {
		return false
	}
	// A backward clock emits and resets rather than suppressing indefinitely.
	*slot = passcodeWarningSlot{nodeDir: nodeDir, last: now, seen: true}
	return true
}

// Only warning frequency changes. Denial/accounting and the error-only callback
// contract remain unchanged. Silence never proves that accounting recovered.
func warnPasscodeLockoutUnavailableFor(mode passcodeWarningMode, nodeDir string, err error) {
	if !errors.Is(err, nodevault.ErrLockoutUnavailable) || !passcodeWarnings.shouldWarn(mode, nodeDir, time.Now) {
		return
	}
	label := "master"
	if mode == passcodeWarningLegacy {
		label = "legacy"
	}
	// Log outside the reminder mutex. Never format a path, PIN or wrapped error.
	slog.Warn("Passcode check denied: lockout state unavailable; inspect node-local passcode status", "mode", label)
}
