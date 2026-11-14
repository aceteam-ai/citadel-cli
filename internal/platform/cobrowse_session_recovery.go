package platform

import (
	"fmt"
	"time"
)

// RecoveryState avoids a page/CDP read: recovery receipts contain no page data.
// Only an observed exit is terminal evidence. An absent/adoption-failed session
// is unknown, even if the older best-effort registry labels it expired.
func (m *CobrowseSessionManager) RecoveryState(id string) string {
	m.Recover()
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		return "unknown"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc == nil {
		return "unknown"
	}
	if s.proc.exited != nil {
		select {
		case <-s.proc.exited:
			return "ended"
		default:
		}
	}
	if s.state == SessionRunning || s.state == SessionAttached {
		return "running"
	}
	return "unknown"
}

// StopRecoverySession confirms browser exit, not merely removal from the session
// map. Keep an unconfirmed session observable for another stop attempt. The
// recovery journal serializes this method with its matching start.
func (m *CobrowseSessionManager) StopRecoverySession(id string) (bool, error) {
	m.Recover()
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		return false, nil
	}
	s.teardownMu.Lock()
	defer s.teardownMu.Unlock()
	s.mu.Lock()
	proc := s.proc
	s.mu.Unlock()
	if proc == nil || proc.exited == nil || proc.stop == nil {
		return false, nil
	}
	// Preserve the process handle for retries until exit is actually observed.
	if err := proc.stop(); err != nil {
		return false, fmt.Errorf("browser exit unconfirmed")
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-proc.exited:
	case <-timer.C:
		return false, fmt.Errorf("browser exit unconfirmed")
	}
	m.mu.Lock()
	if m.sessions[id] == s {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	// Keep sealing/removal in the existing owner, without another process stop.
	if err := s.teardownAfterStop(proc); err != nil {
		return false, fmt.Errorf("browser cleanup unconfirmed")
	}
	return true, nil
}
