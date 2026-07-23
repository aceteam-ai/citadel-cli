// internal/terminal/server.go
package terminal

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Logger is the interface for terminal server logging
type Logger interface {
	Printf(format string, v ...interface{})
	Debugf(format string, v ...interface{})
}

// defaultLogger is the default logger that writes to stderr
type defaultLogger struct {
	logger *log.Logger
	debug  bool
}

func newDefaultLogger(debug bool) *defaultLogger {
	return &defaultLogger{
		logger: log.New(os.Stderr, "[terminal] ", log.LstdFlags),
		debug:  debug,
	}
}

func (l *defaultLogger) Printf(format string, v ...interface{}) {
	l.logger.Printf(format, v...)
}

func (l *defaultLogger) Debugf(format string, v ...interface{}) {
	if l.debug {
		l.logger.Printf("[DEBUG] "+format, v...)
	}
}

// noOpLogger is a silent logger for TUI mode
type noOpLogger struct{}

func (l *noOpLogger) Printf(format string, v ...interface{}) {}
func (l *noOpLogger) Debugf(format string, v ...interface{}) {}

// ctxKey is the private type for request-context keys set by this server.
type ctxKey int

// vpnConnKey marks a request whose underlying connection arrived on a VPN
// (mesh) listener rather than the localhost/LAN bind. It is the signal that
// gates mesh-peer identity trust (citadel #585): only VPN connections may be
// authorized by mesh identity; localhost and any public exposure still require
// a token. It is set by the ConnContext hook installed in Start.
const vpnConnKey ctxKey = iota

// meshTaggedListener wraps a net.Listener whose accepted connections should be
// treated as arriving over the mesh/VPN. Every listener registered via
// AddListener is a tsnet VPN listener (the primary localhost bind in Start is
// created separately and left unwrapped), so AddListener wraps them here. The
// wrapping is what lets the ConnContext hook distinguish a mesh peer (eligible
// for identity trust) from a localhost/LAN client (token required).
type meshTaggedListener struct{ net.Listener }

// Accept tags each accepted connection as mesh-originated.
func (l *meshTaggedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &meshConn{Conn: c}, nil
}

// meshConn marks a connection accepted from a meshTaggedListener so the
// ConnContext hook can flag its request context as mesh-originated.
type meshConn struct{ net.Conn }

// Server is the WebSocket terminal server
type Server struct {
	config   *Config
	sessions *SessionManager
	auth     TokenValidator
	meshAuth MeshIdentityResolver // optional; gates mesh-identity trust (#585)
	limiter  *RateLimiter
	logger   Logger

	httpServer *http.Server
	upgrader   websocket.Upgrader

	mu      sync.RWMutex
	running bool

	// extraListeners are additional net.Listeners the server will also serve on
	// (e.g., a tsnet VPN listener). Added via AddListener before Start.
	extraListeners []net.Listener

	// stopIdleChecker signals the idle checker to stop
	stopIdleChecker chan struct{}

	// Connection tracking for debugging
	totalConnections  int64
	failedConnections int64
	activeConnections int64
}

// NewServer creates a new terminal server
func NewServer(config *Config, auth TokenValidator) *Server {
	s := &Server{
		config:          config,
		sessions:        NewSessionManager(config.MaxConnections),
		auth:            auth,
		limiter:         NewRateLimiter(config.RateLimitRPS, config.RateLimitBurst),
		logger:          newDefaultLogger(config.Debug),
		stopIdleChecker: make(chan struct{}),
	}

	s.upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return s.checkOrigin(r)
		},
	}

	return s
}

// SetSilent switches to a no-op logger to suppress all output.
// Use this in TUI mode to prevent log messages from corrupting the display.
func (s *Server) SetSilent() {
	s.logger = &noOpLogger{}
}

// SetMeshResolver wires the mesh-peer identity resolver used to authorize
// tokenless connections that arrive over the VPN listener (citadel #585). It is
// optional and additive: when nil (the default), the server requires a token on
// every listener exactly as before. When set AND config.TrustMeshPeers is true,
// a tokenless VPN connection whose source resolves to a verified same-owner
// tailnet peer is authorized without a platform-minted token. It never weakens
// the localhost/LAN or token paths. Safe to call before Start.
func (s *Server) SetMeshResolver(r MeshIdentityResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.meshAuth = r
}

// AddListener registers an additional net.Listener that the server will also
// serve on. This enables dual-listen on both LAN and VPN interfaces.
//
// If the server is already running, the listener begins serving immediately.
// This lets callers re-attach a VPN listener after a tsnet reconnect without
// restarting the server (see issue #317). If the server is not yet running,
// the listener is queued and served when Start is called.
func (s *Server) AddListener(ln net.Listener) {
	// Wrap so connections accepted here are tagged as mesh/VPN-originated, which
	// is what makes them eligible for mesh-peer identity trust (citadel #585).
	// The raw localhost bind in Start is intentionally NOT wrapped.
	wrapped := &meshTaggedListener{Listener: ln}

	s.mu.Lock()
	s.extraListeners = append(s.extraListeners, wrapped)
	running := s.running
	httpServer := s.httpServer
	s.mu.Unlock()

	if running && httpServer != nil {
		s.logger.Printf("also listening on %s (VPN, hot-attached)", wrapped.Addr().String())
		go func() {
			if err := httpServer.Serve(wrapped); err != nil && err != http.ErrServerClosed {
				s.logger.Printf("VPN listener error: %v", err)
			}
		}()
	}
}

// RemoveListener drops a previously added extra listener from the server's
// tracking slice so a long-lived session that re-attaches a VPN listener across
// many reconnects does not accumulate dead listener references (issue #317).
// It does not close the listener; the caller owns its lifecycle.
func (s *Server) RemoveListener(ln net.Listener) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, l := range s.extraListeners {
		// AddListener stores a meshTaggedListener wrapper, but callers pass the
		// original (unwrapped) listener to RemoveListener, so match either the
		// stored wrapper or its inner listener.
		inner := l
		if mt, ok := l.(*meshTaggedListener); ok {
			inner = mt.Listener
		}
		if l == ln || inner == ln {
			s.extraListeners = append(s.extraListeners[:i], s.extraListeners[i+1:]...)
			return
		}
	}
}

// NewServerWithDebug creates a new terminal server with debug logging enabled
func NewServerWithDebug(config *Config, auth TokenValidator) *Server {
	config.Debug = true
	return NewServer(config, auth)
}

// checkOrigin validates the WebSocket origin header
func (s *Server) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		s.logger.Debugf("allowing connection without origin header (CLI tool or same-origin)")
		return true // Allow requests without origin (e.g., CLI tools)
	}
	// Allow localhost for development
	if strings.Contains(origin, "localhost") || strings.Contains(origin, "127.0.0.1") {
		s.logger.Debugf("allowing localhost origin: %s", origin)
		return true
	}
	// Allow aceteam.ai domains
	if strings.Contains(origin, "aceteam.ai") {
		s.logger.Debugf("allowing aceteam.ai origin: %s", origin)
		return true
	}
	s.logger.Printf("rejecting origin: %s (not in allowlist)", origin)
	return false
}

// Start starts the terminal server
func (s *Server) Start() error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return ErrServerAlreadyRunning
	}
	s.running = true
	s.mu.Unlock()

	s.logger.Printf("starting terminal server on %s:%d", s.config.Host, s.config.Port)
	s.logger.Debugf("configuration: max_connections=%d, idle_timeout=%v, shell=%s, org_id=%s",
		s.config.MaxConnections, s.config.IdleTimeout, s.config.Shell, s.config.OrgID)

	// Set up HTTP handlers
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/terminal", s.handleWebSocket)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/stats", s.handleStats)

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", s.config.Host, s.config.Port),
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
		// Tag requests whose connection arrived on a VPN (mesh) listener. The
		// same mux serves both the localhost bind and every AddListener'd VPN
		// listener; this hook is the single, reliable place to distinguish them
		// so handleWebSocket can gate mesh-peer identity trust to VPN
		// connections only (citadel #585).
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if _, ok := c.(*meshConn); ok {
				return context.WithValue(ctx, vpnConnKey, true)
			}
			return ctx
		},
	}

	// Start idle session checker
	go s.idleCheckerLoop()

	// Start the HTTP server
	listener, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		s.logger.Printf("failed to start: %v", err)
		return fmt.Errorf("failed to listen on port %d: %w", s.config.Port, err)
	}

	// Get the actual port (useful when port 0 is specified)
	actualAddr := listener.Addr().(*net.TCPAddr)
	s.logger.Printf("listening on %s", actualAddr.String())

	go func() {
		if err := s.httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			s.logger.Printf("server error: %v", err)
		}
	}()

	// Serve on any extra listeners (e.g., tsnet VPN)
	for _, ln := range s.extraListeners {
		ln := ln // capture loop variable
		s.logger.Printf("also listening on %s (VPN)", ln.Addr().String())
		go func() {
			if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
				s.logger.Printf("VPN listener error: %v", err)
			}
		}()
	}

	return nil
}

// Stop gracefully stops the terminal server
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return ErrServerNotRunning
	}
	s.running = false
	s.mu.Unlock()

	s.logger.Printf("stopping terminal server...")

	// Stop the idle checker
	close(s.stopIdleChecker)

	// Stop the rate limiter
	s.limiter.Stop()

	// Close all sessions
	sessionCount := s.sessions.Count()
	s.sessions.CloseAll()
	if sessionCount > 0 {
		s.logger.Printf("closed %d active session(s)", sessionCount)
	}

	// Shutdown HTTP server
	if s.httpServer != nil {
		if err := s.httpServer.Shutdown(ctx); err != nil {
			s.logger.Printf("shutdown error: %v", err)
			return err
		}
	}

	s.logger.Printf("terminal server stopped (total=%d, failed=%d)",
		atomic.LoadInt64(&s.totalConnections), atomic.LoadInt64(&s.failedConnections))
	return nil
}

// writeJSONError writes a JSON-formatted error response
func writeJSONError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":"%s","status":%d}`, message, status)
}

// handleRoot handles requests to the root endpoint
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"service":"citadel-terminal","version":"%s","endpoints":["/terminal","/health","/stats"]}`, s.config.Version)
}

// handleHealth handles health check requests
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok","sessions":%d}`, s.sessions.Count())
}

// handleStats handles stats requests (for debugging)
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok","sessions":%d,"total_connections":%d,"failed_connections":%d,"active_connections":%d,"rate_limit_tracked_ips":%d}`,
		s.sessions.Count(),
		atomic.LoadInt64(&s.totalConnections),
		atomic.LoadInt64(&s.failedConnections),
		atomic.LoadInt64(&s.activeConnections),
		s.limiter.Count())
}

// resolveAuth decides whether a connection is authorized WITHOUT touching the
// ResponseWriter, so the decision is unit-testable with an injected mock mesh
// resolver (no live mesh needed). It implements the citadel #585 auth order:
//
//  1. Token present -> validate it; its result GOVERNS. Valid -> authorize with
//     the token's own UserID; invalid -> reject. This keeps the existing
//     platform/token path completely unchanged, including on the VPN listener
//     where the platform relay dials ws://<vpn_ip>:7860?token=... . Token-FIRST
//     (not mesh-first) is essential: otherwise a relayed per-user token would be
//     discarded in favor of the relay's single mesh login and every user would
//     collapse onto one shared tmux session.
//
//  2. Token absent, connection arrived on the VPN listener, mesh trust is
//     enabled (config.TrustMeshPeers) AND a resolver is wired -> authorize by
//     verified mesh-peer identity, with NO token. This is what makes
//     `citadel connect <name>` work tokenlessly: dialing <vpn_ip>:7860 already
//     proves org tailnet membership. The peer must resolve and be same-owner.
//
//  3. Otherwise -> reject. Fail-safe: a connection is NEVER accepted merely
//     because it looked like it arrived over the VPN (unresolved peer, wrong
//     owner, mesh disabled, no resolver, or token-less on localhost/LAN all
//     fall through to here).
//
// Security posture: mesh-identity trust is gated to the VPN listener; the
// localhost/LAN bind and any public exposure still require a token. authVia is
// "token" or "mesh" on success (for the audit log); on failure the returned
// error is a sentinel suitable for HTTP status mapping.
func (s *Server) resolveAuth(ctx context.Context, token string, overVPN bool, remoteAddr string) (*TokenInfo, string, error) {
	// 1. Token path — preserved, and takes precedence on every listener.
	if token != "" {
		info, err := s.auth.ValidateToken(token, s.config.OrgID)
		if err != nil {
			return nil, "", err
		}
		return info, "token", nil
	}

	// 2. Mesh-identity path — tokenless, VPN-only, opt-outable, resolver-gated.
	s.mu.RLock()
	resolver := s.meshAuth
	s.mu.RUnlock()
	if overVPN && s.config.TrustMeshPeers && resolver != nil {
		id, err := resolver.ResolvePeer(ctx, remoteAddr)
		switch {
		case err != nil:
			// Fail-safe: an unverifiable peer falls through to rejection (it does
			// NOT get an unauthenticated session). Logged at debug — a tokenless
			// probe from a non-peer is expected noise.
			s.logger.Debugf("mesh identity unverified for %s (rejecting tokenless): %v", remoteAddr, err)
		case id == nil || !id.SameOwner:
			s.logger.Printf("mesh peer %s rejected: not a same-owner tailnet member", remoteAddr)
		default:
			// Auditable: this connection was authorized purely on verified mesh
			// identity, with no platform token. Log the peer at Printf so the
			// mesh-trust path is always visible in node logs (citadel #585).
			s.logger.Printf("authorized terminal via mesh-peer identity: node=%q login=%q addr=%s (no token; VPN listener, same-owner)",
				id.NodeName, id.UserID, remoteAddr)
			return &TokenInfo{UserID: id.UserID, OrgID: s.config.OrgID}, "mesh", nil
		}
	}

	// 3. No valid credential — reject.
	return nil, "", ErrInvalidToken
}

// handleWebSocket handles WebSocket upgrade and terminal session
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Get client IP for rate limiting
	ip := getClientIP(r)

	atomic.AddInt64(&s.totalConnections, 1)
	s.logger.Debugf("new connection attempt from %s", ip)

	// Check rate limit
	if !s.limiter.Allow(ip) {
		s.logger.Printf("rate limit exceeded for %s", ip)
		atomic.AddInt64(&s.failedConnections, 1)
		writeJSONError(w, ErrRateLimited.Error(), http.StatusTooManyRequests)
		return
	}

	// Authorize the connection. resolveAuth implements the citadel #585 order:
	// token first (preserves the platform/token path unchanged on every
	// listener), then — only for a tokenless connection that arrived on the VPN
	// listener — verified mesh-peer identity. See resolveAuth for the full
	// contract and security posture.
	token := r.URL.Query().Get("token")
	overVPN, _ := r.Context().Value(vpnConnKey).(bool)
	s.logger.Debugf("authorizing connection from %s (overVPN=%v, token=%v) for org %s",
		ip, overVPN, token != "", s.config.OrgID)

	tokenInfo, authVia, err := s.resolveAuth(r.Context(), token, overVPN, r.RemoteAddr)
	if err != nil {
		s.logger.Printf("auth failed from %s (overVPN=%v): %v", ip, overVPN, err)
		atomic.AddInt64(&s.failedConnections, 1)
		switch {
		case errors.Is(err, ErrInvalidToken), errors.Is(err, ErrTokenExpired):
			writeJSONError(w, err.Error(), http.StatusUnauthorized)
		case errors.Is(err, ErrUnauthorized):
			writeJSONError(w, err.Error(), http.StatusForbidden)
		case errors.Is(err, ErrAuthServiceUnavailable):
			writeJSONError(w, err.Error(), http.StatusServiceUnavailable)
		default:
			writeJSONError(w, "authentication failed", http.StatusUnauthorized)
		}
		return
	}

	s.logger.Debugf("authorized user %s from %s via %s", tokenInfo.UserID, ip, authVia)

	// Passcode gate (aceteam#6524): even after a valid token or same-owner mesh
	// identity, the caller must present the per-node passcode to open a shell.
	// This is the owner-consent factor that makes "console enabled" different
	// from "any org mesh peer can dial :7860 and get a shell". Presented via the
	// ?passcode= query param (a browser WebSocket cannot set request headers on
	// the upgrade), with the X-Citadel-Passcode header honored for non-browser
	// clients. Fails closed. Skipped only when no verifier is wired (e.g. the
	// localhost test server), preserving existing behavior there.
	if s.config.PasscodeVerifier != nil {
		passcode := r.URL.Query().Get("passcode")
		if passcode == "" {
			passcode = r.Header.Get("X-Citadel-Passcode")
		}
		if !s.config.PasscodeVerifier(passcode) {
			s.logger.Printf("passcode gate rejected connection from %s (user %s, via %s)", ip, tokenInfo.UserID, authVia)
			atomic.AddInt64(&s.failedConnections, 1)
			writeJSONError(w, "node passcode required", http.StatusUnauthorized)
			return
		}
	}

	// Check connection limit
	if s.sessions.Count() >= s.config.MaxConnections {
		s.logger.Printf("max connections reached (%d), rejecting %s", s.config.MaxConnections, ip)
		atomic.AddInt64(&s.failedConnections, 1)
		writeJSONError(w, ErrMaxConnectionsReached.Error(), http.StatusServiceUnavailable)
		return
	}

	// Upgrade to WebSocket
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Printf("websocket upgrade failed for %s: %v", ip, err)
		atomic.AddInt64(&s.failedConnections, 1)
		return // Upgrade already sent error response
	}

	// Track active connection
	atomic.AddInt64(&s.activeConnections, 1)
	defer func() {
		conn.Close()
		atomic.AddInt64(&s.activeConnections, -1)
	}()

	// Generate session ID
	sessionID := generateSessionID()

	s.logger.Printf("creating session %s for user %s from %s", sessionID, tokenInfo.UserID, ip)

	// When a persistent named session is configured and tmux is available, back
	// the PTY with `tmux new-session -A -s <name>` so the session survives
	// reconnects; otherwise fall back to a bare shell. The tmux session name is
	// derived per-user from the configured base name so a reconnecting client
	// re-attaches to its own live session (running command, scrollback, cwd all
	// preserved by the tmux server) while staying isolated from other users.
	var tmuxCommand []string
	if !sessionDisabled(s.config.SessionName) {
		tmuxSessionName := sessionNameForUser(s.config.SessionName, tokenInfo.UserID)
		tmuxCommand = sessionCommand(tmuxSessionName, s.config.Shell)
		if tmuxCommand != nil {
			s.logger.Debugf("backing session %s with persistent tmux session %q", sessionID, tmuxSessionName)
		} else {
			// tmux backing is wanted (citadel #585 defaults it ON) but no usable
			// tmux binary resolved. Fall back to a bare, non-persistent shell:
			// the connection still succeeds, it just won't survive a reconnect.
			// Warn (not silent) so the missing re-attach is diagnosable.
			s.logger.Printf("tmux unavailable; session %s falls back to a bare (non-persistent) shell — reconnect re-attach disabled (install tmux, or set CITADEL_TERMINAL_SESSION=none to silence)", sessionID)
		}
	}

	// Create PTY session
	session, err := NewSession(SessionConfig{
		ID:          sessionID,
		UserID:      tokenInfo.UserID,
		OrgID:       tokenInfo.OrgID,
		Shell:       s.config.Shell,
		Command:     tmuxCommand,
		InitialCols: 80,
		InitialRows: 24,
		OnClose: func() {
			s.sessions.Remove(sessionID)
			s.logger.Debugf("session %s removed from manager", sessionID)
		},
	})
	if err != nil {
		s.logger.Printf("failed to create PTY session %s: %v", sessionID, err)
		msg := NewErrorMessage(err.Error())
		data, _ := msg.Marshal()
		conn.WriteMessage(websocket.TextMessage, data)
		return
	}

	// Add session to manager
	if err := s.sessions.Add(session); err != nil {
		s.logger.Printf("failed to add session %s to manager: %v", sessionID, err)
		session.Close()
		msg := NewErrorMessage(err.Error())
		data, _ := msg.Marshal()
		conn.WriteMessage(websocket.TextMessage, data)
		return
	}

	s.logger.Printf("session %s started (user=%s, shell=%s, sessions=%d)",
		sessionID, tokenInfo.UserID, s.config.Shell, s.sessions.Count())

	// Handle the connection
	s.handleConnection(conn, session)

	s.logger.Printf("session %s ended (sessions=%d)", sessionID, s.sessions.Count())
}

// handleConnection manages the bidirectional communication between WebSocket and PTY
func (s *Server) handleConnection(conn *websocket.Conn, session *Session) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer session.Close()

	// Set up ping/pong for connection health
	conn.SetPingHandler(func(appData string) error {
		s.logger.Debugf("received ping from session %s", session.ID)
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
	})

	// PTY -> WebSocket goroutine
	go func() {
		buf := make([]byte, 4096)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				n, err := session.Read(buf)
				if err != nil {
					s.logger.Debugf("PTY read error for session %s: %v", session.ID, err)
					cancel()
					return
				}
				if n > 0 {
					msg := NewOutputMessage(buf[:n])
					data, err := msg.Marshal()
					if err != nil {
						s.logger.Debugf("failed to marshal output for session %s: %v", session.ID, err)
						continue
					}
					if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
						s.logger.Debugf("WebSocket write error for session %s: %v", session.ID, err)
						cancel()
						return
					}
				}
			}
		}
	}()

	// WebSocket -> PTY goroutine (main loop)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			_, data, err := conn.ReadMessage()
			if err != nil {
				// Check if this is a normal close
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					s.logger.Debugf("client disconnected normally from session %s", session.ID)
				} else if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					s.logger.Printf("unexpected WebSocket close for session %s: %v", session.ID, err)
				} else {
					s.logger.Debugf("WebSocket read error for session %s: %v", session.ID, err)
				}
				return
			}

			msg, err := UnmarshalMessage(data)
			if err != nil {
				s.logger.Debugf("invalid message from session %s: %v", session.ID, err)
				continue
			}

			if err := msg.Validate(); err != nil {
				s.logger.Debugf("message validation failed for session %s: %v", session.ID, err)
				errMsg := NewErrorMessage(err.Error())
				errData, _ := errMsg.Marshal()
				conn.WriteMessage(websocket.TextMessage, errData)
				continue
			}

			switch msg.Type {
			case MessageTypeInput:
				if _, err := session.Write(msg.Payload); err != nil {
					s.logger.Debugf("PTY write error for session %s: %v", session.ID, err)
					return
				}

			case MessageTypeResize:
				s.logger.Debugf("resizing session %s to %dx%d", session.ID, msg.Cols, msg.Rows)
				if err := session.Resize(msg.Cols, msg.Rows); err != nil {
					s.logger.Debugf("resize failed for session %s: %v", session.ID, err)
					errMsg := NewErrorMessage(err.Error())
					errData, _ := errMsg.Marshal()
					conn.WriteMessage(websocket.TextMessage, errData)
				}

			case MessageTypePing:
				s.logger.Debugf("received application ping from session %s", session.ID)
				pong := NewPongMessage()
				pongData, _ := pong.Marshal()
				conn.WriteMessage(websocket.TextMessage, pongData)
			}
		}
	}
}

// idleCheckerLoop periodically checks for and closes idle sessions
func (s *Server) idleCheckerLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			closed := s.sessions.CloseIdle(s.config.IdleTimeout)
			if closed > 0 {
				s.logger.Printf("closed %d idle terminal session(s)", closed)
			}
		case <-s.stopIdleChecker:
			return
		}
	}
}

// getClientIP extracts the client IP from the request
func getClientIP(r *http.Request) string {
	// Check for X-Forwarded-For header (for proxied requests)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Take the first IP in the list
		if idx := strings.Index(xff, ","); idx != -1 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}

	// Check for X-Real-IP header
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}

	// Fall back to remote address
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// generateSessionID generates a unique session ID
func generateSessionID() string {
	return fmt.Sprintf("term-%d", time.Now().UnixNano())
}

// IsRunning returns whether the server is currently running
func (s *Server) IsRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.running
}

// SessionCount returns the number of active sessions
func (s *Server) SessionCount() int {
	return s.sessions.Count()
}

// Port returns the configured port
func (s *Server) Port() int {
	return s.config.Port
}

// Stats returns the server statistics
func (s *Server) Stats() (total, failed, active int64) {
	return atomic.LoadInt64(&s.totalConnections),
		atomic.LoadInt64(&s.failedConnections),
		atomic.LoadInt64(&s.activeConnections)
}
