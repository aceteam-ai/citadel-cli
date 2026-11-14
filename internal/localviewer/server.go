// Package localviewer is Citadel local mode's loopback co-browse viewer
// (aceteam#13503 design sections 2.5 and 2.7). It serves a small embedded page
// on 127.0.0.1 where the human watches a session, takes control to log in,
// hands back, and approves or denies the agent's actions.
//
// Every request must pass, in order:
//   - Host is exactly 127.0.0.1:<port> (defeats DNS rebinding);
//   - any Origin is exactly http://127.0.0.1:<port>; state-changing requests
//     and WebSocket upgrades must carry it;
//   - the viewer cookie (HttpOnly, SameSite=Strict), which is only minted by
//     exchanging a single-use code from a link the owner opens in the user's
//     default browser, and is never returned over MCP;
//   - for state changes, the X-Citadel-CSRF header (the WebSocket carries the
//     same token as a query parameter, since browsers cannot set headers there).
//
// No CORS headers are ever sent. The listener never binds anything but
// 127.0.0.1. The co-browse browser itself cannot reach this server: the
// navigation filter refuses loopback addresses and this port.
package localviewer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowsestream"
	"github.com/aceteam-ai/citadel-cli/internal/localaudit"
	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

//go:embed assets/*
var assets embed.FS

// TokenFile is the per-launch file `citadel local viewer` reads to ask the
// running owner for a fresh link.
const TokenFile = "local-viewer.json"

// PrivateDirName is the owner-only subdir of the local-mode state dir holding
// TokenFile and the one-time launch files.
const PrivateDirName = "local-viewer"

const (
	codeTTL    = 2 * time.Minute
	csrfHeader = "X-Citadel-CSRF"
	// A page that polled within pageActiveFor is treated as open, so the
	// owner does not open another tab. Background tabs poll slowly (browsers
	// throttle their timers), hence the generous window.
	pageActiveFor  = 90 * time.Second
	reopenThrottle = 2 * time.Minute
)

// TokenInfo is the content of TokenFile.
type TokenInfo struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

// Config configures a Server.
type Config struct {
	Service *localcobrowse.Service
	// Stream adapts the session manager for the screencast. Nil disables the
	// live view (tests).
	Stream cobrowsestream.Session
	// OpenURL opens a URL in the user's default browser.
	OpenURL func(string) error
	// TokenDir is the local-mode state dir (cmd.localStateDir). TokenFile and the launch files live in
	// its owner-only PrivateDirName subdir. Empty disables Open (tests).
	TokenDir string
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
	// Activity reads the newest audit records for the Activity panel; nil
	// hides the panel.
	Activity func(sessionID string, limit int) ([]localaudit.Record, error)
	// Setup, when set, makes this server the first-run setup page instead of
	// the co-browse viewer: the same listener, guard, cookie, bearer and
	// single-use /open code, with the setup routes in place of the session
	// routes. Service may then be nil.
	Setup *SetupConfig
	// PrivateDir overrides the private subdir name (default PrivateDirName).
	PrivateDir string
	// MaxSessions is reported to the setup page through /api/summary.
	MaxSessions int
}

// Server is the loopback viewer.
type Server struct {
	cfg  Config
	mint string // bearer token for /api/mint

	// cookie and bearer are rotated on every code exchange. The bearer is
	// returned only in the /open response, where the page keeps it in
	// sessionStorage (scoped to this origin, so this port), and every data
	// request must carry it. The cookie alone, which a browser also sends to
	// other 127.0.0.1 ports, unlocks nothing but the static page.
	cookie string
	bearer string

	setupMu sync.Mutex

	mu    sync.Mutex
	codes map[string]time.Time
	// launchFiles maps a code to the one-time launch file that carries it.
	launchFiles map[string]string
	port        int
	lastPoll    time.Time
	lastOpen    time.Time
	ln          net.Listener
	http        *http.Server
	stream      *cobrowsestream.StreamHandler
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}

// New returns a Server with fresh per-launch secrets.
func New(cfg Config) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Server{cfg: cfg, cookie: randHex(32), bearer: randHex(32), mint: randHex(32), codes: map[string]time.Time{}, launchFiles: map[string]string{}}
}

// Start listens on an ephemeral 127.0.0.1 port and serves.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("viewer listen: %w", err)
	}
	return s.Serve(ln)
}

// Serve serves on ln, which must be a 127.0.0.1 listener.
func (s *Server) Serve(ln net.Listener) error {
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		_ = ln.Close()
		return fmt.Errorf("viewer must listen on 127.0.0.1, not %s", ln.Addr())
	}
	s.mu.Lock()
	s.ln = ln
	s.port = addr.Port
	if s.cfg.Stream != nil {
		s.stream = cobrowsestream.NewStreamHandler(cobrowsestream.Config{}, s.cfg.Stream, s.streamOriginOK)
		s.stream.SetSilent()
	}
	s.http = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	srv := s.http
	s.mu.Unlock()
	if err := s.writeTokenFile(); err != nil {
		_ = ln.Close()
		return err
	}
	go func() { _ = srv.Serve(ln) }()
	return nil
}

// Port is the bound port (0 before Serve).
func (s *Server) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

// Close stops the server and removes the token file.
func (s *Server) Close() {
	s.mu.Lock()
	srv, stream := s.http, s.stream
	s.http = nil
	s.mu.Unlock()
	if stream != nil {
		stream.CloseViewers()
	}
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
	if s.cfg.TokenDir != "" {
		_ = os.Remove(filepath.Join(s.privateDir(), TokenFile))
		s.mu.Lock()
		for code := range s.launchFiles {
			s.removeLaunchFileLocked(code)
		}
		s.mu.Unlock()
	}
}

func (s *Server) privateDir() string {
	name := s.cfg.PrivateDir
	if name == "" {
		name = PrivateDirName
	}
	return filepath.Join(s.cfg.TokenDir, name)
}

// reopenHint names the command that gives a fresh link.
func (s *Server) reopenHint() string {
	if s.cfg.Setup != nil {
		return "citadel local setup"
	}
	return "citadel local viewer"
}

func (s *Server) writeTokenFile() error {
	if s.cfg.TokenDir == "" {
		return nil
	}
	if err := platform.EnsurePrivateDir(s.privateDir()); err != nil {
		return fmt.Errorf("viewer token dir: %w", err)
	}
	data, _ := json.Marshal(TokenInfo{Port: s.Port(), Token: s.mint, PID: os.Getpid()})
	tmp := filepath.Join(s.privateDir(), TokenFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.privateDir(), TokenFile))
}

// writeLaunchFile writes a one-time HTML file that forwards the browser to
// link, so the code never appears on a command line (where another local user
// could read it from the process list). The file is in the owner-only dir and
// removed once the code is used or expires.
func (s *Server) writeLaunchFile(code, link string) (string, error) {
	if s.cfg.TokenDir == "" {
		return "", errors.New("no private directory for the viewer launch file")
	}
	if err := platform.EnsurePrivateDir(s.privateDir()); err != nil {
		return "", fmt.Errorf("viewer launch dir: %w", err)
	}
	path := filepath.Join(s.privateDir(), "open-"+randHex(8)+".html")
	page := `<!doctype html><meta charset="utf-8"><meta name="referrer" content="no-referrer">` +
		`<meta http-equiv="refresh" content="0;url=` + link + `"><title>Opening the Citadel viewer</title>` +
		`<p>Opening the Citadel viewer&hellip; <a href="` + link + `">Continue</a></p>`
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(page); err != nil {
		f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	s.mu.Lock()
	s.launchFiles[code] = path
	s.mu.Unlock()
	return path, nil
}

// removeLaunchFileLocked deletes the launch file for code. Caller holds s.mu.
func (s *Server) removeLaunchFileLocked(code string) {
	if p, ok := s.launchFiles[code]; ok {
		_ = os.Remove(p)
		delete(s.launchFiles, code)
	}
}

func (s *Server) origin() string { return "http://127.0.0.1:" + strconv.Itoa(s.Port()) }
func (s *Server) host() string   { return "127.0.0.1:" + strconv.Itoa(s.Port()) }

// MintURL returns a fresh single-use link that sets the viewer cookie.
func (s *Server) MintURL() string {
	_, link := s.mint2()
	return link
}

func (s *Server) mint2() (code, link string) {
	code = randHex(24)
	s.mu.Lock()
	now := s.cfg.Now()
	for c, exp := range s.codes {
		if now.After(exp) {
			delete(s.codes, c)
			s.removeLaunchFileLocked(c)
		}
	}
	s.codes[code] = now.Add(codeTTL)
	s.mu.Unlock()
	return code, s.origin() + "/open?code=" + code
}

// MintLaunchFile returns a fresh one-time launch file path (to open in the
// default browser) and the link it forwards to.
func (s *Server) MintLaunchFile() (path, link string, err error) {
	code, link := s.mint2()
	path, err = s.writeLaunchFile(code, link)
	return path, link, err
}

// Open opens the viewer in the user's default browser through a one-time
// launch file; the command line carries only the file's path.
func (s *Server) Open() error {
	if s.cfg.OpenURL == nil {
		return errors.New("no browser opener configured")
	}
	path, _, err := s.MintLaunchFile()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.lastOpen = s.cfg.Now()
	s.mu.Unlock()
	return s.cfg.OpenURL(path)
}

// Attention is the service's OnAttention hook. If a viewer page is already
// open (it polled recently) the page shows the card or banner itself;
// otherwise a viewer is opened, at most once per reopenThrottle.
func (s *Server) Attention(sessionID, reason string) {
	s.mu.Lock()
	now := s.cfg.Now()
	active := now.Sub(s.lastPoll) < pageActiveFor
	recent := !s.lastOpen.IsZero() && now.Sub(s.lastOpen) < reopenThrottle
	s.mu.Unlock()
	if active || recent {
		return
	}
	_ = s.Open()
}

func (s *Server) streamOriginOK(r *http.Request) bool {
	return r.Header.Get("Origin") == s.origin()
}

func forbid(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(msg + "\n"))
}

func setSecurityHeaders(w http.ResponseWriter, origin string) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' blob:; "+
		"connect-src 'self' ws://"+strings.TrimPrefix(origin, "http://")+"; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
}

func (s *Server) cookieName() string { return "citadel_viewer_" + strconv.Itoa(s.Port()) }

func (s *Server) secrets() (cookie, bearer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cookie, s.bearer
}

func (s *Server) hasCookie(r *http.Request) bool {
	want, _ := s.secrets()
	c, err := r.Cookie(s.cookieName())
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(want)) == 1
}

func (s *Server) bearerOK(got string) bool {
	_, want := s.secrets()
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// Handler is the full, guarded mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /open", s.handleOpen)
	mux.HandleFunc("GET /{$}", s.withCookie(s.handleIndex))
	mux.HandleFunc("GET /assets/{file}", s.withCookie(s.handleAsset))
	if s.cfg.Setup != nil {
		s.setupRoutes(mux)
		return s.guard(mux)
	}
	mux.HandleFunc("GET /api/state", s.withCookie(s.withCSRF(s.handleState)))
	mux.HandleFunc("POST /api/sessions/{id}/{action}", s.withCookie(s.withCSRF(s.handleAction)))
	mux.HandleFunc("GET /api/stream", s.withCookie(s.handleStream))
	mux.HandleFunc("GET /api/sessions/{id}/commit/{commit}/shot", s.withCookie(s.withCSRF(s.handleCommitShot)))
	mux.HandleFunc("GET /api/activity", s.withCookie(s.withCSRF(s.handleActivity)))
	mux.HandleFunc("POST /api/mint", s.handleMint)
	mux.HandleFunc("GET /api/summary", s.handleSummary)
	return s.guard(mux)
}

// guard enforces Host and Origin on every request.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w, s.origin())
		if r.Host != s.host() {
			forbid(w, "wrong host")
			return
		}
		origin := r.Header.Get("Origin")
		if origin != "" && origin != s.origin() {
			forbid(w, "foreign origin")
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			// The one exception: the top-level navigation from the one-time
			// launch file (a file: page) to /open. The code is its auth.
			openNav := r.Method == http.MethodGet && r.URL.Path == "/open" && r.Header.Get("Sec-Fetch-Mode") == "navigate"
			if !openNav {
				forbid(w, "cross-site request")
				return
			}
		}
		stateChanging := r.Method != http.MethodGet && r.Method != http.MethodHead
		isWS := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
		if (stateChanging || isWS) && origin == "" && r.URL.Path != "/api/mint" {
			forbid(w, "missing origin")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withCookie(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.hasCookie(r) {
			forbid(w, "This Citadel link has expired or was opened in another browser. Run `"+s.reopenHint()+"` for a new one.")
			return
		}
		next(w, r)
	}
}

func (s *Server) withCSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.bearerOK(r.Header.Get(csrfHeader)) {
			forbid(w, "missing or wrong viewer token")
			return
		}
		next(w, r)
	}
}

// handleOpen exchanges a single-use code for a fresh viewer cookie and a fresh
// bearer, invalidating the previous ones. A cookie is never issued without a
// valid, unused, unexpired code. The bearer goes only into this response's
// page, which stores it in sessionStorage and moves to "/" by script (a
// same-origin navigation, so the SameSite=Strict cookie is sent).
func (s *Server) handleOpen(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	s.mu.Lock()
	exp, ok := s.codes[code]
	delete(s.codes, code)
	s.removeLaunchFileLocked(code)
	valid := code != "" && ok && !s.cfg.Now().After(exp)
	var cookie, bearer string
	stream := s.stream
	if valid {
		s.cookie, s.bearer = randHex(32), randHex(32)
		cookie, bearer = s.cookie, s.bearer
	}
	s.mu.Unlock()
	if valid && stream != nil {
		// The old tab's stream holds the single-viewer claim and its token is
		// now revoked: drop it, so the new tab can attach right away.
		stream.CloseViewers()
	}
	if !valid {
		forbid(w, "This Citadel link has expired or was already used. Run `"+s.reopenHint()+"` for a new one.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    cookie,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	page, err := assets.ReadFile("assets/open.html")
	if err != nil {
		http.Error(w, "missing page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(bytes.ReplaceAll(page, []byte("{{BEARER}}"), []byte(bearer)))
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	name := "assets/index.html"
	if s.cfg.Setup != nil {
		name = "assets/setup.html"
	}
	page, err := assets.ReadFile(name)
	if err != nil {
		http.Error(w, "missing page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	var ctype string
	switch name {
	case "app.js", "open.js", "setup.js":
		ctype = "text/javascript; charset=utf-8"
	case "app.css":
		ctype = "text/css; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	data, err := assets.ReadFile("assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	_, _ = w.Write(data)
}

// State is GET /api/state.
type State struct {
	Agent    string                      `json:"agent"`
	Sessions []localcobrowse.SessionView `json:"sessions"`
	Now      string                      `json:"now"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.lastPoll = s.cfg.Now()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, State{
		Agent:    s.cfg.Service.Agent(),
		Sessions: s.cfg.Service.Sessions(""),
		Now:      s.cfg.Now().UTC().Format(time.RFC3339),
	})
}

// handleActivity serves the Activity panel: the session's newest audit records.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Activity == nil {
		writeJSON(w, http.StatusOK, map[string]any{"records": []localaudit.Record{}})
		return
	}
	recs, err := s.cfg.Activity(r.URL.Query().Get("session"), 50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "the audit log could not be read"})
		return
	}
	if recs == nil {
		recs = []localaudit.Record{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": recs})
}

// handleCommitShot serves the pending send/post/submit/pay request's
// screenshot and target box for the approval card. It is no-store: the picture
// is the page the user is being asked about.
func (s *Server) handleCommitShot(w http.ResponseWriter, r *http.Request) {
	png, rect, ok := s.cfg.Service.CommitShot(r.PathValue("id"), r.PathValue("commit"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"png": png, "rect": rect})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	id, action := r.PathValue("id"), r.PathValue("action")
	var body struct {
		Hours  float64 `json:"hours"`
		URL    string  `json:"url"`
		Domain string  `json:"domain"`
		Commit string  `json:"commit"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
	}
	svc := s.cfg.Service
	var err error
	switch action {
	case "approve":
		err = svc.ApproveDomain(id, body.Domain, time.Duration(body.Hours*float64(time.Hour)))
	case "approve-commit":
		err = svc.ApproveCommit(id, body.Commit)
	case "deny-commit":
		err = svc.DenyCommit(id, body.Commit)
	case "deny":
		err = svc.Deny(id)
	case "revoke":
		err = svc.Revoke(id)
	case "take-control":
		err = svc.HumanTakeControl(id)
	case "hand-back":
		err = svc.HumanHandBack(id)
	case "stop":
		err = svc.HumanStop(id)
	case "navigate":
		err = svc.HumanNavigate(r.Context(), id, body.URL)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if !s.bearerOK(r.URL.Query().Get("csrf")) {
		forbid(w, "missing or wrong viewer token")
		return
	}
	if r.Header.Get("Origin") != s.origin() {
		forbid(w, "missing origin")
		return
	}
	s.mu.Lock()
	stream := s.stream
	s.mu.Unlock()
	if stream == nil {
		http.Error(w, "live view unavailable", http.StatusServiceUnavailable)
		return
	}
	stream.ServeHTTP(w, r)
}

// handleMint is for `citadel local viewer`, a separate process of the same
// user: it proves itself with the per-launch bearer token from TokenFile.
// Browser requests (which carry an Origin) are refused.
func (s *Server) handleMint(w http.ResponseWriter, r *http.Request) {
	if !s.mintAuthOK(r) {
		forbid(w, "bad token")
		return
	}
	path, link, err := s.MintLaunchFile()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"url": link})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": link, "file": path})
}

// mintAuthOK is true for a request from another local process holding the
// per-launch token. A browser request (which carries an Origin) never is.
func (s *Server) mintAuthOK(r *http.Request) bool {
	if r.Header.Get("Origin") != "" {
		return false
	}
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(auth), []byte(s.mint)) == 1
}

// RequestLink asks a running viewer (described by the TokenFile in dir) for a
// fresh single-use link. Loopback only.
func RequestLink(ctx context.Context, dir string, client *http.Client) (string, error) {
	link, _, err := RequestLaunch(ctx, dir, client)
	return link, err
}

// RequestLaunch is RequestLink that also returns the one-time launch file to
// open (empty if the viewer could not write one).
func RequestLaunch(ctx context.Context, dir string, client *http.Client) (link, file string, err error) {
	data, err := os.ReadFile(filepath.Join(dir, PrivateDirName, TokenFile))
	if err != nil {
		return "", "", fmt.Errorf("no Citadel local co-browse is running on this computer (start a session from your agent first)")
	}
	var info TokenInfo
	if err := json.Unmarshal(data, &info); err != nil || info.Port <= 0 || info.Token == "" {
		return "", "", fmt.Errorf("the viewer token file is unreadable; restart your agent app")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(info.Port)+"/api/mint", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+info.Token)
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("the Citadel process that owned co-browse is no longer running (start a session from your agent first)")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("the running viewer refused the request (%s)", resp.Status)
	}
	var out struct {
		URL  string `json:"url"`
		File string `json:"file"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.URL == "" {
		return "", "", fmt.Errorf("unexpected viewer response")
	}
	return out.URL, out.File, nil
}
