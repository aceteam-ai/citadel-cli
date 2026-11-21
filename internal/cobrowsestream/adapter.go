package cobrowsestream

import (
	"context"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

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
	return NewManagerSessionFrom(platform.GetCobrowseSessionManager())
}

// NewManagerSessionFrom wraps a specific manager (e.g. an isolated one in tests).
//
// Page tracking (TargetSession) is offered only when the manager has it enabled
// (local or new remote sessions). A manager without it yields the
// plain Session: no targets frames, no page switching, the first page streamed.
func NewManagerSessionFrom(m *platform.CobrowseSessionManager) Session {
	if m != nil && m.TargetsEnabled() {
		return targetManagerSession{managerSession{m: m}}
	}
	// File attach is offered only by a manager that was given a Files
	// permission source and does not track pages (the node-wide manager).
	if m != nil && m.HumanFileAttachSupported() {
		return fileAttachManagerSession{managerSession{m: m}}
	}
	return managerSession{m: m}
}

type targetManagerSession struct{ managerSession }

func (s targetManagerSession) TracksTargets(id string) bool { return s.m.SessionTargetsEnabled(id) }

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

func (s managerSession) MarkAttached(id string) bool     { return s.m.MarkAttached(id) }
func (s managerSession) MarkDetached(id string) bool     { return s.m.MarkDetached(id) }
func (s managerSession) ViewerHasControl(id string) bool { return s.m.ViewerHasControl(id) }

// Targets implements TargetSession.
func (s targetManagerSession) Targets(id string) (TargetSnapshot, error) {
	t, err := s.m.Targets(id)
	if err != nil {
		return TargetSnapshot{}, err
	}
	snap := TargetSnapshot{Active: t.Active, ActiveWebSocketURL: t.ActiveWebSocketURL}
	for _, tg := range t.Targets {
		snap.Targets = append(snap.Targets, TargetInfo{ID: tg.ID, Title: tg.Title, URL: tg.URL, Active: tg.Active, Refused: tg.Refused, OpenedBy: tg.OpenedBy})
	}
	return snap, nil
}

func (s targetManagerSession) SwitchTarget(id, target string) error {
	return s.m.SwitchTarget(id, target)
}
func (s targetManagerSession) CloseTarget(id, target string) error {
	return s.m.CloseTarget(id, target)
}

type fileAttachManagerSession struct{ managerSession }

// FileChooser implements FileAttachSession.
func (s fileAttachManagerSession) FileChooser(id string) (FileChooserMessage, bool) {
	o, ok := s.m.HumanFileAttachOffer(id)
	if !ok {
		return FileChooserMessage{}, false
	}
	msg := FileChooserMessage{Type: FileChooserType, Pending: true, BackendNodeID: o.BackendNodeID, Multiple: o.Multiple, Staged: []StagedFileInfo{}}
	for _, f := range o.Staged {
		msg.Staged = append(msg.Staged, StagedFileInfo{Name: f.Name, Size: f.Size})
	}
	return msg, true
}

// AttachStagedFile implements FileAttachSession. Only a fixed reason code
// leaves this function.
func (s fileAttachManagerSession) AttachStagedFile(ctx context.Context, id string, backendNodeID int, stagedName string) string {
	_, err := s.m.HumanAttachStagedFile(ctx, id, backendNodeID, stagedName)
	return platform.AttachFailureReason(err)
}
