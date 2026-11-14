package platform

import (
	"fmt"
	"os"
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
	// A confirmed process stop is not repeated just because profile sealing or
	// directory removal needs another attempt. Keep its exact handle until all
	// cleanup stages succeed; a concurrent legacy teardown uses teardownMu too.
	if !s.recoveryStopped {
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
		s.recoveryStopped = true
	}
	if err := s.finishRecoveryCleanup(); err != nil {
		return false, fmt.Errorf("browser cleanup unconfirmed")
	}
	m.mu.Lock()
	if m.sessions[id] == s {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	return true, nil
}

// Caller holds teardownMu after verified process exit. Persist before closing or
// deleting anything. Completed stages survive retries in this owner; a restart
// that loses the unlocked profile handle remains unknown, never falsely clean.
func (s *cobrowseSession) finishRecoveryCleanup() error {
	s.mu.Lock()
	enc, profile, regDir := s.encProfile, s.profile, s.regDir
	s.mu.Unlock()
	if enc != nil && !s.recoveryPersisted {
		if err := enc.Persist(profile); err != nil {
			return err
		}
		s.recoveryPersisted = true
	}
	if enc != nil && !s.recoveryClosed {
		if err := enc.Close(); err != nil {
			return err
		}
		s.recoveryClosed = true
	}
	// removeSessionDir is best effort for legacy teardown. Recovery must inspect
	// the actual error and retain this session for another cleanup attempt.
	if profile != "" {
		if err := os.RemoveAll(profile); err != nil {
			return err
		}
		if _, err := os.Lstat(profile); !os.IsNotExist(err) {
			return fmt.Errorf("profile removal unconfirmed")
		}
	}
	if regDir != "" {
		deleteRecord(regDir, s.id)
	}
	s.mu.Lock()
	s.proc = nil
	s.encProfile = nil
	s.mu.Unlock()
	return nil
}
