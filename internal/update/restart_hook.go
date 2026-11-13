package update

import "sync"

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
