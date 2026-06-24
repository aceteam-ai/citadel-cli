// Package power provides cross-platform, CGO-free sleep-inhibition and
// power-source detection for keeping a Citadel node awake while it serves the
// Fabric. It deliberately avoids cgo (the repo builds with CGO_ENABLED=0) by
// shelling out to OS utilities (caffeinate, systemd-inhibit) or calling pure-Go
// syscalls (SetThreadExecutionState on Windows).
//
// The inhibition assertion is always scoped to the holding process: on macOS
// and Linux it is a child process that the OS reaps if Citadel crashes, and on
// Windows it is cleared on the calling thread / process exit. This means we can
// never leave a machine permanently un-sleepable.
package power

// Source identifies the current power source of the machine.
type Source int

const (
	// SourceUnknown means the power source could not be determined (e.g. a
	// desktop or VM with no battery/AC sysfs entries). Treated as "not AC" by
	// the gating logic so we never inhibit sleep when we cannot prove the
	// machine is plugged in.
	SourceUnknown Source = iota
	// SourceAC means the machine is running on external/AC power.
	SourceAC
	// SourceBattery means the machine is running on battery.
	SourceBattery
)

// String renders the source for logs and the TUI.
func (s Source) String() string {
	switch s {
	case SourceAC:
		return "AC"
	case SourceBattery:
		return "battery"
	default:
		return "unknown"
	}
}

// Inhibitor holds an OS sleep-inhibition assertion for the lifetime of the
// process. Start is idempotent (a second call while already active is a no-op)
// and Stop releases the assertion. Implementations must be safe to Stop even if
// Start failed or was never called.
type Inhibitor interface {
	// Start acquires the assertion. Returns an error if the OS mechanism could
	// not be engaged; callers should treat that as "sleep not inhibited".
	Start() error
	// Stop releases the assertion cleanly. Safe to call multiple times.
	Stop() error
	// Active reports whether the assertion is currently held.
	Active() bool
}

// ShouldInhibit is the pure gating decision: we only hold a sleep assertion
// when the operator has opted in AND the machine is provably on AC power.
// Battery and Unknown both return false so an unplugged (or indeterminate)
// laptop is never kept awake.
func ShouldInhibit(enabled bool, src Source) bool {
	return enabled && src == SourceAC
}
