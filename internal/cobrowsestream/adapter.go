package cobrowsestream

import "github.com/aceteam-ai/citadel-cli/internal/platform"

// managerSession adapts the #793 CobrowseSessionManager to the Session interface
// this server needs, using only the manager's EXISTING exported API so the
// manager file needs no edits. AttachTarget is derived from SessionStatus; the
// attach/detach state hooks the manager left for #794 are wired straight through.
type managerSession struct {
	m *platform.CobrowseSessionManager
}

// NewManagerSession returns a Session backed by the process-wide co-browse
// session manager. This is the production binding used by `citadel work`.
func NewManagerSession() Session {
	return managerSession{m: platform.GetCobrowseSessionManager()}
}

// NewManagerSessionFrom wraps a specific manager (e.g. an isolated one in tests).
func NewManagerSessionFrom(m *platform.CobrowseSessionManager) Session {
	return managerSession{m: m}
}

// AttachTarget returns the session's CDP debug port when it is attachable: it
// must be known, have a debug port, and be in a running/attached state (not
// still launching, and not exited/crashed).
func (s managerSession) AttachTarget(id string) (int, bool) {
	st, ok := s.m.SessionStatus(id)
	if !ok || st.DebugPort == 0 {
		return 0, false
	}
	if st.State != platform.SessionRunning && st.State != platform.SessionAttached {
		return 0, false
	}
	return st.DebugPort, true
}

func (s managerSession) MarkAttached(id string) bool { return s.m.MarkAttached(id) }
func (s managerSession) MarkDetached(id string) bool { return s.m.MarkDetached(id) }
