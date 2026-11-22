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

	"rsc.io/qr"
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

	// EnrollStart begins connecting this PC to AceTeam (citadel-cli#1867 C2).
	// It must return at once; the enrollment runs in the background. Its
	// error is shown to the user as is, so it must be fixed copy, never error
	// text from the enrollment. The three Enroll hooks are set together; nil
	// hides the Connect to AceTeam section and its routes answer 404.
	EnrollStart func() (SetupEnrollState, error)
	// EnrollState reports the current enrollment.
	EnrollState func() SetupEnrollState
	// EnrollCancel asks a running enrollment to stop.
	EnrollCancel func() SetupEnrollState

	// StartWorker keeps this PC available to AceTeam after a connect: it turns
	// on sign-in autostart and starts the background worker
	// (citadel-cli#1867 C5). Nil answers 404. Its error is fixed copy.
	StartWorker func() (SetupActionResult, error)
	// Disconnect stops the background worker, removes autostart and logs this
	// PC out of the AceTeam Network, reporting each step. Local mode keeps
	// working and saved logins are never touched. Nil answers 404.
	Disconnect func() (SetupActionResult, error)
}

// SetupStep is one step of a setup action and how it went. Status is "ok",
// "failed", "skipped" or "not_applicable"; Message is fixed copy.
type SetupStep struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// SetupActionResult is a multi-step setup action: every step it ran, in
// order, and a summary. OK is true only when no step failed.
type SetupActionResult struct {
	OK      bool        `json:"ok"`
	Steps   []SetupStep `json:"steps"`
	Message string      `json:"message"`
}

// SetupEnrollState is what the page shows of a "Connect to AceTeam"
// enrollment: display fields and fixed copy only. The type has no field for
// the device code or any credential, so no response can carry one.
type SetupEnrollState struct {
	// Stage is idle, starting, waiting (the code is shown), connecting,
	// connected, already (this PC already has a registration), canceling,
	// canceled or failed.
	Stage string `json:"stage"`
	// UserCode and VerificationURL are what the user approves with.
	UserCode        string `json:"user_code,omitempty"`
	VerificationURL string `json:"verification_url,omitempty"`
	OrgName         string `json:"org_name,omitempty"`
	NodeName        string `json:"node_name,omitempty"`
	// Failure is the failure class (for example "denied"); Message is the
	// fixed copy shown for the stage or failure.
	Failure string `json:"failure,omitempty"`
	Message string `json:"message,omitempty"`
	// Posture is fixed copy describing what AceTeam agents may do on this PC
	// after a connect; WorkerOffer shows "Keep this PC available to AceTeam".
	Posture     string `json:"posture,omitempty"`
	WorkerOffer bool   `json:"worker_offer,omitempty"`
}

type setupState struct {
	Checks       []SetupCheck   `json:"checks"`
	Apps         []SetupApp     `json:"apps"`
	Sessions     []SetupSession `json:"sessions"`
	SessionsNote string         `json:"sessions_note,omitempty"`
	MaxSessions  int            `json:"max_sessions"`
	GenericEntry string         `json:"generic_entry"`
	// EnrollAvailable shows the Connect to AceTeam section.
	EnrollAvailable bool `json:"enroll_available"`
	// DisconnectAvailable shows the Disconnect this PC button.
	DisconnectAvailable bool `json:"disconnect_available"`
}

func (s *Server) setupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/setup/state", s.withCookie(s.withCSRF(s.handleSetupState)))
	mux.HandleFunc("POST /api/setup/doctor", s.withCookie(s.withCSRF(s.handleSetupDoctor)))
	mux.HandleFunc("POST /api/setup/connect/{app}", s.withCookie(s.withCSRF(s.handleSetupConnect)))
	mux.HandleFunc("POST /api/setup/open-viewer", s.withCookie(s.withCSRF(s.handleSetupOpenViewer)))
	mux.HandleFunc("POST /api/setup/enroll/start", s.withCookie(s.withCSRF(s.handleSetupEnrollStart)))
	mux.HandleFunc("GET /api/setup/enroll/state", s.withCookie(s.withCSRF(s.handleSetupEnrollState)))
	mux.HandleFunc("POST /api/setup/enroll/cancel", s.withCookie(s.withCSRF(s.handleSetupEnrollCancel)))
	mux.HandleFunc("GET /api/setup/enroll/qr", s.withCookie(s.withCSRF(s.handleSetupEnrollQR)))
	mux.HandleFunc("POST /api/setup/worker/start", s.withCookie(s.withCSRF(s.handleSetupAction(func(c *SetupConfig) func() (SetupActionResult, error) { return c.StartWorker }))))
	mux.HandleFunc("POST /api/setup/disconnect", s.withCookie(s.withCSRF(s.handleSetupAction(func(c *SetupConfig) func() (SetupActionResult, error) { return c.Disconnect }))))
}

// handleSetupAction runs one multi-step action after a JSON confirmation. A
// failed step is still 200 with OK false, so the page can show every step;
// an error (for example, a refusal before any step ran) is 409.
func (s *Server) handleSetupAction(pick func(*SetupConfig) func() (SetupActionResult, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		run := pick(s.cfg.Setup)
		if run == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "this action is not available here"})
			return
		}
		if !readConfirm(w, r) {
			return
		}
		s.setupMu.Lock()
		defer s.setupMu.Unlock()
		res, err := run()
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "result": res})
			return
		}
		writeJSON(w, http.StatusOK, res)
	}
}

func (s *Server) handleSetupState(w http.ResponseWriter, r *http.Request) {
	c := s.cfg.Setup
	st := setupState{MaxSessions: c.MaxSessions, GenericEntry: c.GenericEntry, Sessions: []SetupSession{}, EnrollAvailable: c.enrollAvailable(), DisconnectAvailable: c.Disconnect != nil}
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

func (c *SetupConfig) enrollAvailable() bool {
	return c.EnrollStart != nil && c.EnrollState != nil && c.EnrollCancel != nil
}

// readConfirm requires a JSON body with "confirm": true, like the Connect
// click, so only the page's own buttons start or stop an enrollment.
func readConfirm(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "expected JSON"})
		return false
	}
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || !body.Confirm {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the click must confirm the action"})
		return false
	}
	return true
}

func (s *Server) enrollUnavailable(w http.ResponseWriter) bool {
	if s.cfg.Setup.enrollAvailable() {
		return false
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "connecting to AceTeam is not available here"})
	return true
}

// handleSetupEnrollStart begins one enrollment. A start while one runs gets
// 409 with the running enrollment's state, which it leaves unchanged.
func (s *Server) handleSetupEnrollStart(w http.ResponseWriter, r *http.Request) {
	if s.enrollUnavailable(w) || !readConfirm(w, r) {
		return
	}
	st, err := s.cfg.Setup.EnrollStart()
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "enroll": st})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleSetupEnrollState(w http.ResponseWriter, r *http.Request) {
	if s.enrollUnavailable(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.cfg.Setup.EnrollState())
}

func (s *Server) handleSetupEnrollCancel(w http.ResponseWriter, r *http.Request) {
	if s.enrollUnavailable(w) || !readConfirm(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.cfg.Setup.EnrollCancel())
}

// handleSetupEnrollQR serves the waiting enrollment's verification URL as a
// PNG QR code, for approving from a phone. The page fetches it with the bearer
// header and shows it through a blob: URL, which the CSP allows.
func (s *Server) handleSetupEnrollQR(w http.ResponseWriter, r *http.Request) {
	if s.enrollUnavailable(w) {
		return
	}
	st := s.cfg.Setup.EnrollState()
	if st.Stage != "waiting" || st.VerificationURL == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no code is waiting for approval"})
		return
	}
	code, err := qr.Encode(st.VerificationURL, qr.M)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not draw the QR code"})
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(code.PNG())
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
