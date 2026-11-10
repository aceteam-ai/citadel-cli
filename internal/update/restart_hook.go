package update

import "sync"

var (
	beforeRestartMu   sync.Mutex
	beforeRestartHook func(detail string)
)

// SetBeforeRestartHook registers a function RestartProcess runs just before the
// process replaces itself. The hook receives the staged version (for example
// "to v1.2.3") when known, else "". It must be fast and must not panic.
func SetBeforeRestartHook(h func(detail string)) {
	beforeRestartMu.Lock()
	beforeRestartHook = h
	beforeRestartMu.Unlock()
}

func runBeforeRestart() {
	beforeRestartMu.Lock()
	h := beforeRestartHook
	beforeRestartMu.Unlock()
	if h == nil {
		return
	}
	defer func() { _ = recover() }()
	detail := ""
	if st, corrupt := ReadStateForReport(); st != nil && !corrupt && st.CurrentVersion != "" {
		detail = "to " + st.CurrentVersion
	}
	h(detail)
}
