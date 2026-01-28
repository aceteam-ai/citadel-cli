// internal/terminal/server.go
package terminal

import (
	"context"
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

// Server is the WebSocket terminal server
type Server struct {
	config   *Config
	sessions *SessionManager
	auth     TokenValidator
	limiter  *RateLimiter
	logger   *defaultLogger

	httpServer *http.Server
	upgrader   websocket.Upgrader

	mu      sync.RWMutex
	running bool

	// stopIdleChecker signals the idle checker to stop
	stopIdleChecker chan struct{}

	// Connection tracking for debugging
	totalConnections  int64
	failedConnections int64
	activeConnections int64
}

// NewServer creates a new terminal server
func NewServer(config *Config, auth TokenValidator) *Server {
	logger := newDefaultLogger(config.Debug)
	s := &Server{
		config:          config,
		sessions:        NewSessionManager(config.MaxConnections),
		auth:            auth,
		limiter:         NewRateLimiter(config.RateLimitRPS, config.RateLimitBurst),
		logger:          logger,
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

	// Get and validate token
	token := r.URL.Query().Get("token")
	if token == "" {
		s.logger.Printf("missing token from %s", ip)
		atomic.AddInt64(&s.failedConnections, 1)
		writeJSONError(w, "missing token parameter", http.StatusUnauthorized)
		return
	}

	// Log token validation attempt (don't log the actual token for security)
	s.logger.Debugf("validating token for org %s from %s", s.config.OrgID, ip)

	tokenInfo, err := s.auth.ValidateToken(token, s.config.OrgID)
	if err != nil {
		s.logger.Printf("token validation failed from %s: %v", ip, err)
		atomic.AddInt64(&s.failedConnections, 1)
		switch err {
		case ErrInvalidToken, ErrTokenExpired:
			writeJSONError(w, err.Error(), http.StatusUnauthorized)
		case ErrUnauthorized:
			writeJSONError(w, err.Error(), http.StatusForbidden)
		case ErrAuthServiceUnavailable:
			writeJSONError(w, err.Error(), http.StatusServiceUnavailable)
		default:
			writeJSONError(w, "authentication failed", http.StatusUnauthorized)
		}
		return
	}

	s.logger.Debugf("token validated for user %s from %s", tokenInfo.UserID, ip)

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

	// Create PTY session
	session, err := NewSession(SessionConfig{
		ID:          sessionID,
		UserID:      tokenInfo.UserID,
		OrgID:       tokenInfo.OrgID,
		Shell:       s.config.Shell,
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
