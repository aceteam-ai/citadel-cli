package platform

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// RecoveryState avoids a page/CDP read: recovery receipts contain no page data.
// Only an observed exit is terminal evidence. An absent/adoption-failed session
// is unknown, even if the older best-effort registry labels it expired, unless
// this process confirmed its exit and cleanup on another stop path.
func (m *CobrowseSessionManager) RecoveryState(id string) string {
	m.Recover()
	m.mu.Lock()
	s := m.sessions[id]
	_, done := m.recoveryDone[id]
	m.mu.Unlock()
	if s == nil {
		if done {
			return "ended"
		}
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

// RecoveryPresent reports whether a session holds id in this manager, whatever
// its state. A journal without a record checks it before writing a tombstone.
func (m *CobrowseSessionManager) RecoveryPresent(id string) bool {
	m.Recover()
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.sessions[id]
	return ok
}

// StopRecoverySession confirms browser exit, not merely removal from the session
// map. Keep an unconfirmed session observable for another stop attempt. The
// recovery journal serializes this method with its matching start.
func (m *CobrowseSessionManager) StopRecoverySession(id string) (bool, error) {
	m.Recover()
	return m.stopRecoveryCustody(id)
}

// errRecoveryStopUnconfirmed is what a non-recovery stop path (user stop,
// shutdown, public-only sweep, egress-guard failure) returns for a recovery
// session whose exit or cleanup could not be confirmed. The session stays
// under recovery custody for the journal's retry.
var errRecoveryStopUnconfirmed = errors.New("co-browse session stop unconfirmed; it stays under recovery custody")

// stopRecoveryCustody stops a recovery-scoped session in the map, or reports
// earlier confirmed evidence when it already left. On success the session
// leaves the map and its handle is recorded as done.
func (m *CobrowseSessionManager) stopRecoveryCustody(id string) (bool, error) {
	m.mu.Lock()
	s := m.sessions[id]
	_, done := m.recoveryDone[id]
	m.mu.Unlock()
	if s == nil {
		return done, nil
	}
	confirmed, err := s.recoveryStop()
	if confirmed {
		m.mu.Lock()
		if m.sessions[id] == s {
			delete(m.sessions, id)
		}
		m.markRecoveryDoneLocked(id)
		m.mu.Unlock()
	}
	return confirmed, err
}

func (m *CobrowseSessionManager) markRecoveryDoneLocked(id string) {
	if m.recoveryDone == nil {
		m.recoveryDone = make(map[string]struct{})
	}
	m.recoveryDone[id] = struct{}{}
}

// stopRecoveryScoped is Stop for a recovery handle: the same custody stop the
// journal uses, so a user or guard stop never strands the journal record.
func (m *CobrowseSessionManager) stopRecoveryScoped(id string) error {
	confirmed, err := m.stopRecoveryCustody(id)
	if err != nil {
		return err
	}
	m.mu.Lock()
	_, present := m.sessions[id]
	m.mu.Unlock()
	if !confirmed && present {
		return errRecoveryStopUnconfirmed
	}
	return nil
}

// stopRemoved stops a session already taken out of the map by a shutdown path.
// A recovery session goes through recovery custody; if that cannot confirm the
// stop it is put back, so this process still owns it for a retry.
func (m *CobrowseSessionManager) stopRemoved(s *cobrowseSession) error {
	if !recoveryScopedID(s.id) {
		return s.teardown()
	}
	confirmed, err := s.recoveryStop()
	m.mu.Lock()
	defer m.mu.Unlock()
	if confirmed {
		m.markRecoveryDoneLocked(s.id)
		return nil
	}
	if _, taken := m.sessions[s.id]; !taken {
		m.sessions[s.id] = s
	}
	if err == nil {
		err = errRecoveryStopUnconfirmed
	}
	return err
}

// recoveryStop is the custody stop of one session. It never removes the
// session from the map; callers do that only when it reports confirmed.
func (s *cobrowseSession) recoveryStop() (bool, error) {
	s.teardownMu.Lock()
	defer s.teardownMu.Unlock()
	s.mu.Lock()
	proc := s.proc
	enc := s.encProfile
	s.mu.Unlock()
	if proc == nil || proc.exited == nil || proc.stop == nil {
		return false, nil
	}
	// The same first step as teardown: no chooser fill and no new stage once a
	// stop was requested, and staged files are removed before anything else.
	s.quiesceStaging()
	// A confirmed process stop is not repeated just because profile sealing or
	// directory removal needs another attempt. Keep its exact handle until all
	// cleanup stages succeed; a concurrent legacy teardown uses teardownMu too.
	if !s.recoveryStopped {
		if enc != nil {
			// As in teardown: let Chromium flush cookie and login writes before
			// the stop, so a later seal does not lose them.
			closeBrowserGracefully(proc)
		}
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
		dropTargetSet(proc.debugPort)
	}
	if err := s.finishRecoveryCleanup(proc); err != nil {
		return false, fmt.Errorf("browser cleanup unconfirmed")
	}
	return true, nil
}

// Caller holds teardownMu after verified process exit. It applies teardown's
// sealing rules (settleBeforeSeal, sealDir, removeProfileWorkDir) but keeps
// custody: any failure returns before the record, process handle or unlocked
// profile is released, so another attempt can finish. Completed stages survive
// retries in this owner; a restart that loses the unlocked profile handle
// remains unknown, never falsely clean.
func (s *cobrowseSession) finishRecoveryCleanup(proc *cobrowseProc) error {
	s.mu.Lock()
	enc, profile, regDir := s.encProfile, s.profile, s.regDir
	src := sealDir(profile, s.workDir)
	s.mu.Unlock()
	if !s.recoveryPersisted {
		// Nothing is sealed while a helper still uses the profile or staged
		// files remain; both are rechecked on every attempt until sealed.
		reapErr, stageErr := s.settleBeforeSeal(proc)
		if reapErr != nil {
			return reapErr
		}
		if stageErr != nil {
			return stageErr
		}
		if enc != nil {
			if err := enc.Persist(src); err != nil {
				return err
			}
			s.recoveryPersisted = true
		}
	}
	if enc != nil && !s.recoveryClosed {
		if err := enc.Close(); err != nil {
			return err
		}
		s.recoveryClosed = true
	}
	// Teardown's removal is best effort for a throwaway session. Recovery must
	// see every removal or reap failure and retain this session for retry.
	if profile != "" {
		rerr, reapErr := removeProfileWorkDir(profile, proc)
		if reapErr != nil {
			return reapErr
		}
		if rerr != nil {
			return rerr
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
