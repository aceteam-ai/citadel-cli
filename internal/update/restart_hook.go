package update

import (
	"sync"
	"sync/atomic"
)

// RestartReasonNodeUpdate is the reason for a restart onto a new binary.
const RestartReasonNodeUpdate = "node_update"

var (
	beforeRestartMu   sync.Mutex
	beforeRestartHook func(reason, detail string)
)

// RestartProcess re-execs (or relaunches) onto the installed binary after an
// update. On success it does not return.
func RestartProcess() error { return restartProcess(RestartReasonNodeUpdate) }

// RestartProcessWithReason is RestartProcess for a restart that is not a binary
// update (for example an external engine change), so co-browse status can say why.
func RestartProcessWithReason(reason string) error { return restartProcess(reason) }

// SetBeforeRestartHook registers a function the restart runs just before the
// process replaces itself. It receives the reason and, for an update, the staged
// version (for example "to v1.2.3"). It must be fast and must not panic.
func SetBeforeRestartHook(h func(reason, detail string)) {
	beforeRestartMu.Lock()
	beforeRestartHook = h
	beforeRestartMu.Unlock()
}

func runBeforeRestart(reason string) {
	beforeRestartMu.Lock()
	h := beforeRestartHook
	beforeRestartMu.Unlock()
	if h == nil {
		return
	}
	defer func() { _ = recover() }()
	detail := ""
	if reason == RestartReasonNodeUpdate {
		if st, corrupt := ReadStateForReport(); st != nil && !corrupt && st.CurrentVersion != "" {
			detail = "to " + st.CurrentVersion
		}
	}
	h(reason, detail)
}

// SupervisorRestartExitCode is the exit code a worker run by the per-user
// supervisor (`citadel work --background`, Windows) uses to ask it for an
// immediate relaunch onto the installed binary. The supervisor reads the same
// constant, so the two cannot drift.
const SupervisorRestartExitCode = 75

var supervisedRestart atomic.Bool

// SetSupervisedRestart marks this process as a worker started by the per-user
// supervisor. The Windows restart then always exits with
// SupervisorRestartExitCode and never spawns a copy of itself: a copy would
// race the supervisor's relaunch for the worker lock, and if the supervisor
// misread that exit as a clean stop its job would kill the copy, leaving no
// worker. Set it from the supervisor's flag alone, not from a lock probe that
// could fail. Unix keeps execve, which preserves the PID.
func SetSupervisedRestart(on bool) { supervisedRestart.Store(on) }

func restartBySupervisor() bool { return supervisedRestart.Load() }
