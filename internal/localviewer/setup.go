package localviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// SetupDirName is the private subdir used by the setup server, kept apart from
// PrivateDirName so a setup process never overwrites the running co-browse
// owner's TokenFile.
const SetupDirName = "local-setup"

// SetupCheck is one doctor result shown on the setup page. Status is "ok",
// "note" or "FAIL".
type SetupCheck struct {
	Status string `json:"status"`
	What   string `json:"what"`
	Detail string `json:"detail"`
}

// SetupApp is one agent app. State is "connected", "available" or "not_found".
type SetupApp struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	Path  string `json:"path"`
	State string `json:"state"`
	// Note is a caveat such as an unverified location.
	Note string `json:"note,omitempty"`
}

// SetupSession is a live co-browse session as the owner reports it. Domain is
// the origin only.
type SetupSession struct {
	ID     string `json:"id"`
	Goal   string `json:"goal,omitempty"`
	Driver string `json:"driver"`
	Domain string `json:"domain,omitempty"`
	State  string `json:"state"`
}

// SetupConnectResult is what a Connect click did.
type SetupConnectResult struct {
	// Outcome is "written", "already" or "declined".
	Outcome  string `json:"outcome"`
	Path     string `json:"path"`
	Backup   bool   `json:"backup"`
	NextStep string `json:"next_step"`
	Message  string `json:"message"`
}

// SetupConfig turns a Server into the first-run setup page. Every behavior is
// injected so the package stays free of command code.
type SetupConfig struct {
	// Doctor runs the checks; full also starts a hidden test browser.
	Doctor func(full bool) []SetupCheck
	Apps   func() []SetupApp
	// Connect merges the entry into the named app's config. expectedPath is the
	// file the page showed next to the button; Connect must refuse if the
	// app's real path differs, so the click only consents to what was shown.
	Connect func(name, expectedPath string) (SetupConnectResult, error)
	// Sessions lists the live sessions of the running co-browse owner.
	Sessions func() ([]SetupSession, error)
	// OpenViewer opens the owner's handoff viewer in the default browser.
	OpenViewer func() error
	// MaxSessions is shown read-only.
	MaxSessions int
	// GenericEntry is a copyable MCP entry for apps Citadel does not know.
	GenericEntry string
}

type setupState struct {
	Checks       []SetupCheck   `json:"checks"`
	Apps         []SetupApp     `json:"apps"`
	Sessions     []SetupSession `json:"sessions"`
	SessionsNote string         `json:"sessions_note,omitempty"`
	MaxSessions  int            `json:"max_sessions"`
	GenericEntry string         `json:"generic_entry"`
}

func (s *Server) setupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/setup/state", s.withCookie(s.withCSRF(s.handleSetupState)))
	mux.HandleFunc("POST /api/setup/doctor", s.withCookie(s.withCSRF(s.handleSetupDoctor)))
	mux.HandleFunc("POST /api/setup/connect/{app}", s.withCookie(s.withCSRF(s.handleSetupConnect)))
	mux.HandleFunc("POST /api/setup/open-viewer", s.withCookie(s.withCSRF(s.handleSetupOpenViewer)))
}

func (s *Server) handleSetupState(w http.ResponseWriter, r *http.Request) {
	c := s.cfg.Setup
	st := setupState{MaxSessions: c.MaxSessions, GenericEntry: c.GenericEntry, Sessions: []SetupSession{}}
	if c.Doctor != nil {
		st.Checks = c.Doctor(false)
	}
	if c.Apps != nil {
		st.Apps = c.Apps()
	}
	if c.Sessions != nil {
		if sess, err := c.Sessions(); err != nil {
			st.SessionsNote = err.Error()
		} else if sess != nil {
			st.Sessions = sess
		}
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleSetupDoctor(w http.ResponseWriter, r *http.Request) {
	checks := []SetupCheck{}
	if s.cfg.Setup.Doctor != nil {
		checks = s.cfg.Setup.Doctor(true)
	}
	writeJSON(w, http.StatusOK, map[string]any{"checks": checks})
}

// handleSetupConnect is the state-changing action. The guard already required
// Host, Origin and a same-origin fetch; this adds the cookie, the viewer token
// header (a custom header no cross-site form can send) and an explicit JSON
// confirmation naming the file the user was shown.
func (s *Server) handleSetupConnect(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "expected JSON"})
		return
	}
	var body struct {
		Confirm bool   `json:"confirm"`
		Path    string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || !body.Confirm || body.Path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the click must confirm the file shown on the page"})
		return
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	res, err := s.cfg.Setup.Connect(r.PathValue("app"), body.Path)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleSetupOpenViewer(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Setup.OpenViewer == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the viewer is not available"})
		return
	}
	if err := s.cfg.Setup.OpenViewer(); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type summaryResponse struct {
	Sessions    []SetupSession `json:"sessions"`
	MaxSessions int            `json:"max_sessions"`
}

// handleSummary lets another process of the same user (the setup page) list
// the live sessions. It uses the same per-launch token as /api/mint, and
// browsers (which send an Origin) are refused.
func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	if !s.mintAuthOK(r) {
		forbid(w, "bad token")
		return
	}
	out := summaryResponse{Sessions: []SetupSession{}, MaxSessions: s.cfg.MaxSessions}
	if s.cfg.Service != nil {
		for _, v := range s.cfg.Service.Sessions("") {
			out.Sessions = append(out.Sessions, SetupSession{ID: v.SessionID, Goal: v.Goal, Driver: v.Driver, Domain: originOf(v.URL), State: v.State})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// RequestSummary asks the running owner (described by the TokenFile in dir)
// for its live sessions and session limit.
func RequestSummary(ctx context.Context, dir string, client *http.Client) ([]SetupSession, int, error) {
	data, err := os.ReadFile(filepath.Join(dir, PrivateDirName, TokenFile))
	if err != nil {
		return nil, 0, fmt.Errorf("no agent app is using co-browse right now")
	}
	var info TokenInfo
	if err := json.Unmarshal(data, &info); err != nil || info.Port <= 0 || info.Token == "" {
		return nil, 0, fmt.Errorf("the viewer token file is unreadable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(info.Port)+"/api/summary", nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+info.Token)
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("no agent app is using co-browse right now")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("the running co-browse refused the request (%s)", resp.Status)
	}
	var out summaryResponse
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, 0, fmt.Errorf("unexpected co-browse response")
	}
	return out.Sessions, out.MaxSessions, nil
}
