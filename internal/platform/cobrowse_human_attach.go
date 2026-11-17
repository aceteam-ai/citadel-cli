// Human file attach for co-browse sessions (citadel-cli#1723, follow-up to the
// agent's set_file_input in #1721).
//
// While a person has live control through the viewer stream they can answer the
// page's pending file chooser with a file already staged in this session's
// staging dir. It is a stream input, not a job action, an MCP tool or an HTTP
// route: the only caller is the viewer handler, behind its per-input
// ViewerHasControl gate. No agent surface reaches HumanAttachStagedFile.
//
// It shares the dispatch path with set_file_input (fillFileInput: isolated
// world, checkFileInput, setFilesIfPageIs, pathRedacted errors). Two things
// differ. The agent's "refused while a human drives" becomes "requires the
// viewer to have control", re-checked right before DOM.setFileInputFiles. The
// agent's expected_url becomes the binding to the pending chooser: the
// message's node id must be the chooser's, and the chooser frame's URL, read
// from the browser when the chooser opened, must still be the frame's URL at
// dispatch.
package platform

import (
	"errors"
	"log"
	"os"
	"sort"
)

// Human attach refusals. Each is a fixed reason; none carries a path, URL or
// content.
var (
	ErrViewerNoControl      = errors.New("the viewer does not have control of this session")
	ErrAttachFilesDisabled  = errors.New("file attach is not permitted on this node")
	ErrNoPendingChooser     = errors.New("no file chooser is pending")
	ErrChooserMismatch      = errors.New("that is not the pending file chooser")
	errAttachNotApplicable  = errors.New("file attach is not available for this session")
	errAttachChooserMissing = errors.New("the chooser's frame URL is not known")
)

// SetFilesAllowed wires the live Files permission the viewer's file attach
// follows (the same switch as stage_file). It is read on every offer and again
// immediately before dispatch. Unset denies.
func (m *CobrowseSessionManager) SetFilesAllowed(f func() bool) {
	m.mu.Lock()
	m.filesAllowed = f
	m.mu.Unlock()
}

func (m *CobrowseSessionManager) filesPermitted() bool {
	m.mu.Lock()
	f := m.filesAllowed
	m.mu.Unlock()
	return f != nil && f()
}

// HumanFileAttachSupported reports whether this manager can offer the viewer a
// file attach: it must have a Files permission source and must not track pages
// (local mode has neither a chooser watcher nor a Files permission).
func (m *CobrowseSessionManager) HumanFileAttachSupported() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.filesAllowed != nil && !m.trackTargets
}

// StagedFileInfo is what the viewer is shown of a staged file: name and size.
type StagedFileInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// FileAttachOffer is the pending chooser and the files the viewer may pick.
type FileAttachOffer struct {
	BackendNodeID int
	Multiple      bool
	Staged        []StagedFileInfo
}

// HumanFileAttachOffer reports the pending chooser and the staged files, only
// while the viewer has control, Files is permitted and the chooser is ready to
// be bound (its frame URL is known). It reads names and sizes only, never
// content, and lists nothing outside the session's staging dir.
func (m *CobrowseSessionManager) HumanFileAttachOffer(id string) (FileAttachOffer, bool) {
	if !m.filesPermitted() || !m.ViewerHasControl(id) {
		return FileAttachOffer{}, false
	}
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil || s.tracking {
		return FileAttachOffer{}, false
	}
	s.mu.Lock()
	var offer FileAttachOffer
	ready := s.chooser != nil && s.chooser.FrameURL != "" && s.chooser.FrameID != ""
	if ready {
		offer.BackendNodeID, offer.Multiple = s.chooser.BackendNodeID, s.chooser.Multiple
	}
	s.mu.Unlock()
	if !ready {
		return FileAttachOffer{}, false
	}
	offer.Staged = s.listStaged()
	return offer, true
}

// listStaged lists regular files directly inside the staging dir with a valid
// staged name. A missing or unsafe dir lists nothing.
func (s *cobrowseSession) listStaged() []StagedFileInfo {
	dir, err := s.existingStagingDir()
	if err != nil {
		return []StagedFileInfo{}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return []StagedFileInfo{}
	}
	out := []StagedFileInfo{}
	for _, e := range ents {
		if ValidateStagedName(e.Name()) != nil {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, StagedFileInfo{Name: e.Name(), Size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// humanControlFence refuses unless the live process the attach was started on
// is still running and the viewer still has control. It is the human path's
// counterpart of driverFence and sits immediately before the dispatch.
func (s *cobrowseSession) humanControlFence(port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc == nil {
		return ErrNotStarted
	}
	select {
	case <-s.proc.exited:
		return ErrNotStarted
	default:
	}
	if s.proc.debugPort != port {
		return ErrNotStarted
	}
	if s.state != SessionAttached || !s.explicitHandoff {
		return ErrViewerNoControl
	}
	return nil
}

// HumanAttachStagedFile fills the pending file chooser's input with one staged
// file for the viewer that has control. The chooser must be the one named by
// backendNodeID, and its frame must still be at the URL it had when the chooser
// opened. On any refusal nothing reaches the browser. The audit line carries
// actor, name, size and sha256, never bytes or a path.
//
// Serialization: an attach holds attachMu, so human attaches never overlap.
// Nothing else serializes it against agent actions (set_file_input included):
// the node manager has no per-session action lock. The two sides are kept apart
// only by the driver flag explicitHandoff: agent actions refuse while it is set,
// this attach refuses unless it is set, and each re-checks it under s.mu right
// before dispatching. Neither holds a lock across the dispatch, so a handoff or
// resume landing between one side's last check and its DOM.setFileInputFiles
// can let both dispatch. That window exists for every agent action already
// (driverFence); this path does not widen it.
func (m *CobrowseSessionManager) HumanAttachStagedFile(id string, backendNodeID int, stagedName string) (StagedFile, error) {
	if !m.filesPermitted() {
		return StagedFile{}, ErrAttachFilesDisabled
	}
	if backendNodeID <= 0 {
		return StagedFile{}, ErrChooserMismatch
	}
	s, err := m.sessionForAction(id)
	if err != nil {
		return StagedFile{}, err
	}
	if s.tracking {
		return StagedFile{}, errAttachNotApplicable
	}
	s.attachMu.Lock()
	defer s.attachMu.Unlock()

	// The viewer must have control, and the browser must be up.
	s.mu.Lock()
	var port int
	running := s.proc != nil
	if running {
		port = s.proc.debugPort
	}
	control := s.state == SessionAttached && s.explicitHandoff
	chooser := s.chooser
	var bound FileChooserPending
	if chooser != nil {
		bound = *chooser
	}
	s.mu.Unlock()
	if !running {
		return StagedFile{}, ErrNotStarted
	}
	if !control {
		return StagedFile{}, ErrViewerNoControl
	}
	// Binding: only the chooser the page opened, never an arbitrary node.
	if chooser == nil {
		return StagedFile{}, ErrNoPendingChooser
	}
	if bound.BackendNodeID != backendNodeID {
		return StagedFile{}, ErrChooserMismatch
	}
	if bound.FrameID == "" || bound.FrameURL == "" {
		return StagedFile{}, errAttachChooserMissing
	}
	// Resolve and hash before any CDP call, so a refused path never reaches
	// the browser.
	path, file, err := s.resolveStagedFile(stagedName)
	if err != nil {
		return StagedFile{}, err
	}

	b, cancel := newActionBudget()
	defer cancel()
	p := pin{port: port}
	conn, err := openCDPConn(b.act, p)
	if err != nil {
		return StagedFile{}, err
	}
	defer conn.Close()
	var eff ActionEffects
	err = s.fillFileInput(conn, &eff, fileFill{
		backendNodeID: backendNodeID,
		chooserFrame:  bound.FrameID,
		expectedURL:   bound.FrameURL,
		path:          path,
		file:          file,
		fence: func() error {
			// The lookups await the page: the chooser must still be the
			// pending one (a main-frame navigation clears it).
			s.mu.Lock()
			same := s.chooser == chooser
			s.mu.Unlock()
			if !same {
				return ErrTargetChanged
			}
			return s.humanControlFence(port)
		},
		driverFence: func() error {
			if !m.filesPermitted() {
				return ErrAttachFilesDisabled
			}
			return s.humanControlFence(port)
		},
	})
	if err != nil {
		return StagedFile{}, err
	}
	log.Printf("[cobrowse] audit file_attach actor=human session=%s name=%q size=%d sha256=%s", id, file.Name, file.Size, file.SHA256)
	return file, nil
}

// AttachFailureReason maps a human attach error to a fixed, generic reason
// code. The text never carries a path, URL, name or content.
func AttachFailureReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrViewerNoControl):
		return "no_control"
	case errors.Is(err, ErrAttachFilesDisabled):
		return "files_disabled"
	case errors.Is(err, ErrNoPendingChooser):
		return "no_pending_chooser"
	case errors.Is(err, ErrChooserMismatch):
		return "chooser_mismatch"
	case errors.Is(err, ErrStagedNameInvalid):
		return "invalid_staged_name"
	case errors.Is(err, ErrStagedPathRefused):
		return "staged_file_unavailable"
	case errors.Is(err, ErrTargetChanged), errors.Is(err, errAttachChooserMissing):
		return "page_changed"
	case errors.Is(err, ErrNotStarted), errors.Is(err, errAttachNotApplicable):
		return "session_unavailable"
	default:
		return "attach_failed"
	}
}
