package localviewer

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Node permission toggles (citadel-cli#1867 C5). A PC connected to AceTeam from
// the setup page runs co-browse only; these routes let the human at this PC
// allow Files or Shell when a task needs it. They are served by the setup page
// and the local viewer, behind the same guard, cookie and bearer as every other
// state change, and need a JSON confirmation; turning Shell on needs a second,
// separate confirmation field. No MCP tool and no remote job reaches them.

// Node permission surfaces the page may change.
const (
	SurfaceFiles = "files"
	SurfaceShell = "shell"
)

// NodePermissionState is what the page shows of this PC's AceTeam permissions.
type NodePermissionState struct {
	// Available is false when this PC is not connected to AceTeam; the page
	// then hides the toggles.
	Available bool `json:"available"`
	Files     bool `json:"files"`
	Shell     bool `json:"shell"`
	// Console (terminal) and Desktop (screen) are shown, not changed here.
	Console bool `json:"console"`
	Desktop bool `json:"desktop"`
	// Passcode reports a node passcode or master PIN, without which an enabled
	// Shell still refuses every command.
	Passcode bool `json:"passcode"`
	// PublicOnly is the co-browse public-only egress guard.
	PublicOnly bool `json:"public_only"`
	// Workspace is the folder Files covers.
	Workspace string `json:"workspace,omitempty"`
	// Message is fixed copy about the state (for example, why the toggles are
	// unavailable).
	Message string `json:"message,omitempty"`
}

// NodePermissionHooks reads and changes this PC's node permissions. Both are
// required; nil hooks leave the routes answering 404.
type NodePermissionHooks struct {
	Get func() (NodePermissionState, error)
	// Set changes one surface. Its error is shown as is, so it must be fixed
	// copy.
	Set func(surface string, enabled bool) (NodePermissionState, error)
}

func (h *NodePermissionHooks) available() bool {
	return h != nil && h.Get != nil && h.Set != nil
}

func (s *Server) nodePermissionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/node-permissions", s.withCookie(s.withCSRF(s.handleNodePermissions)))
	mux.HandleFunc("POST /api/node-permissions", s.withCookie(s.withCSRF(s.handleNodePermissionSet)))
}

func (s *Server) handleNodePermissions(w http.ResponseWriter, r *http.Request) {
	h := s.cfg.NodePermissions
	if !h.available() {
		writeJSON(w, http.StatusOK, NodePermissionState{})
		return
	}
	st, err := h.Get()
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleNodePermissionSet changes one surface. The body must carry
// "confirm": true, and turning Shell on also "confirm_shell": true, which only
// the page's second confirmation sends.
func (s *Server) handleNodePermissionSet(w http.ResponseWriter, r *http.Request) {
	h := s.cfg.NodePermissions
	if !h.available() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "permissions cannot be changed here"})
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "expected JSON"})
		return
	}
	var body struct {
		Confirm      bool   `json:"confirm"`
		ConfirmShell bool   `json:"confirm_shell"`
		Surface      string `json:"surface"`
		Enabled      *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || !body.Confirm || body.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the click must confirm the change"})
		return
	}
	if body.Surface != SurfaceFiles && body.Surface != SurfaceShell {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown permission"})
		return
	}
	if body.Surface == SurfaceShell && *body.Enabled && !body.ConfirmShell {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "allowing commands needs the second confirmation"})
		return
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	st, err := h.Set(body.Surface, *body.Enabled)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "permissions": st})
		return
	}
	writeJSON(w, http.StatusOK, st)
}
