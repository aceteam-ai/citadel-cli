// internal/platform/cobrowse_ended.go
//
// Why a session ended (citadel-cli#1546). Before a self-update restart the
// process records every live session in an owner-only marker in the sessions
// base dir. A session that is re-adopted afterwards is removed from it; one
// that did not survive stays, so status can answer with ended_reason instead of
// a bare "no such browser session".
package platform

import (
	"fmt"
	"os"
	"sort"
	"time"
)

// EndedSession is a recorded end (or, as PendingEnd, an intended one).
type EndedSession struct {
	ID     string `json:"id,omitempty"`
	Reason string `json:"ended_reason"`
	Detail string `json:"detail,omitempty"`
	At     string `json:"at,omitempty"`
}

// SessionEndedError is returned for an action on a session that ended with a
// recorded reason.
type SessionEndedError struct {
	ID, Reason, Detail string
}

func (e *SessionEndedError) Error() string {
	d := ""
	if e.Detail != "" {
		d = " " + e.Detail
	}
	return fmt.Sprintf("browser session %q ended: ended_reason=%s%s", e.ID, e.Reason, d)
}

// EndedSession looks up a session that ended with a recorded reason.
func (m *CobrowseSessionManager) EndedSession(id string) (EndedSession, bool) {
	if !sessionIDPattern.MatchString(id) {
		return EndedSession{}, false
	}
	dir := m.registryDir()
	if _, err := os.Lstat(dir); err != nil || ensureCobrowseBaseDir(dir) != nil {
		return EndedSession{}, false
	}
	rec, err := loadRecord(dir, id)
	if err != nil || rec.State != RecordExpired || rec.EndedReason == "" {
		return EndedSession{}, false
	}
	if t, err := time.Parse(time.RFC3339, rec.EndedAt); err != nil || time.Since(t) >= expiredRetention {
		return EndedSession{}, false
	}
	return EndedSession{ID: id, Reason: rec.EndedReason, Detail: rec.EndedDetail, At: rec.EndedAt}, true
}

// BusySession names a session that should not be interrupted by a restart.
type BusySession struct{ ID, Why string }

// BusySessions lists sessions with a viewer attached, a handoff outstanding, or
// a human driving.
func (m *CobrowseSessionManager) BusySessions() []BusySession {
	m.mu.Lock()
	all := make([]*cobrowseSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	var out []BusySession
	for _, s := range all {
		s.mu.Lock()
		switch {
		case s.explicitHandoff:
			out = append(out, BusySession{s.id, "handoff outstanding"})
		case s.state == SessionAttached:
			out = append(out, BusySession{s.id, "viewer attached"})
		}
		s.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// CobrowseBusy reports whether a restart would interrupt a human-involved
// co-browse session in this process: a multi-session one, or the legacy single
// browser while handed off to the human.
func CobrowseBusy() (bool, string) {
	if b := GetCobrowseSessionManager().BusySessions(); len(b) > 0 {
		return true, fmt.Sprintf("co-browse session %s: %s", b[0].ID, b[0].Why)
	}
	if st := GetCobrowseManager().Status(); st.Running && st.Driver == DriverHuman {
		return true, "co-browse browser handed off to a human"
	}
	return false, ""
}

// PrepareSelfRestart runs just before the process replaces itself. It records
// why sessions may end (see prepareSelfRestart) and, where nothing can be
// re-adopted, stops the legacy single browser too so none is orphaned.
func PrepareSelfRestart(reason, detail string) {
	GetCobrowseSessionManager().prepareSelfRestart(reason, detail)
	if !adoptionSupported {
		_ = GetCobrowseManager().Stop()
	}
}

// prepareSelfRestart decides each session's fate across the restart.
//   - Persistent (encrypted profile) sessions are sealed and stopped on EVERY OS
//     while their key is still unlocked: the single-session lock is in memory,
//     so a surviving one would let a second session open the same profile.
//   - Where adoption is unsupported every session is stopped cleanly.
//   - Other sessions are left running with their record stamped with the reason
//     they will end for if they do not survive.
//
// Every stopped session is recorded as ended with the reason, on every OS.
func (m *CobrowseSessionManager) prepareSelfRestart(reason, detail string) {
	m.mu.Lock()
	all := make([]*cobrowseSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		s.mu.Lock()
		stop := !adoptionSupported || s.persistent || s.encProfile != nil
		s.mu.Unlock()
		if !stop {
			_ = m.MarkSessionEnding(s, reason, detail)
			continue
		}
		_ = m.Stop(s.id) // seals a persistent profile, closes its lock, removes the dir
		m.recordEnded(s.id, reason, detail)
	}
}

// MarkSessionEnding stamps one live session's record with its pending end.
func (m *CobrowseSessionManager) MarkSessionEnding(s *cobrowseSession, reason, detail string) error {
	s.mu.Lock()
	s.pendingEnd = &EndedSession{Reason: reason, Detail: detail, At: time.Now().UTC().Format(time.RFC3339)}
	s.mu.Unlock()
	s.persistRecord()
	return nil
}

// recordEnded writes an expired record so status can say why a stopped session
// is gone. Works on every OS; a no-op for managers without a registry.
func (m *CobrowseSessionManager) recordEnded(id, reason, detail string) {
	if !m.useResolver {
		return
	}
	_ = saveRecord(m.registryDir(), sessionRecord{
		Version: sessionRecordVersion, ID: id, State: RecordExpired,
		EndedReason: reason, EndedDetail: detail, EndedAt: time.Now().UTC().Format(time.RFC3339),
	})
}
