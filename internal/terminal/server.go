// internal/terminal/server.go
package terminal

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Server is the WebSocket terminal server
type Server struct {
	config   *Config
	sessions *SessionManager
	auth     TokenValidator
	limiter  *RateLimiter

	httpServer *http.Server
	upgrader   websocket.Upgrader

	mu      sync.RWMutex
	running bool

	// stopIdleChecker signals the idle checker to stop
	stopIdleChecker chan struct{}
}

// NewServer creates a new terminal server
func NewServer(config *Config, auth TokenValidator) *Server {
	return &Server{
		config:   config,
		sessions: NewSessionManager(config.MaxConnections),
		auth:     auth,
		limiter:  NewRateLimiter(config.RateLimitRPS, config.RateLimitBurst),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				// Allow connections from aceteam.ai domains
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true // Allow requests without origin (e.g., CLI tools)
				}
				// Allow localhost for development
				if strings.Contains(origin, "localhost") || strings.Contains(origin, "127.0.0.1") {
					return true
				}
				// Allow aceteam.ai domains
				if strings.Contains(origin, "aceteam.ai") {
					return true
				}
				return false
			},
		},
		stopIdleChecker: make(chan struct{}),
	}
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

	// Set up HTTP handlers
	mux := http.NewServeMux()
	mux.HandleFunc("/terminal", s.handleWebSocket)
	mux.HandleFunc("/health", s.handleHealth)

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.config.Port),
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
		return fmt.Errorf("failed to listen on port %d: %w", s.config.Port, err)
	}

	go func() {
		if err := s.httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Printf("Terminal server error: %v\n", err)
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

	// Stop the idle checker
	close(s.stopIdleChecker)

	// Stop the rate limiter
	s.limiter.Stop()

	// Close all sessions
	s.sessions.CloseAll()

	// Shutdown HTTP server
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}

	return nil
}

// handleHealth handles health check requests
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok","sessions":%d}`, s.sessions.Count())
}

// handleWebSocket handles WebSocket upgrade and terminal session
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Get client IP for rate limiting
	ip := getClientIP(r)

	// Check rate limit
	if !s.limiter.Allow(ip) {
		http.Error(w, ErrRateLimited.Error(), http.StatusTooManyRequests)
		return
	}

	// Get and validate token
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "missing token parameter", http.StatusUnauthorized)
		return
	}

	tokenInfo, err := s.auth.ValidateToken(token, s.config.OrgID)
	if err != nil {
		switch err {
		case ErrInvalidToken, ErrTokenExpired:
			http.Error(w, err.Error(), http.StatusUnauthorized)
		case ErrUnauthorized:
			http.Error(w, err.Error(), http.StatusForbidden)
		case ErrAuthServiceUnavailable:
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		default:
			http.Error(w, "authentication failed", http.StatusUnauthorized)
		}
		return
	}

	// Check connection limit
	if s.sessions.Count() >= s.config.MaxConnections {
		http.Error(w, ErrMaxConnectionsReached.Error(), http.StatusServiceUnavailable)
		return
	}

	// Upgrade to WebSocket
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already sent error response
	}
	defer conn.Close()

	// Generate session ID
	sessionID := generateSessionID()

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
		},
	})
	if err != nil {
		msg := NewErrorMessage(err.Error())
		data, _ := msg.Marshal()
		conn.WriteMessage(websocket.TextMessage, data)
		return
	}

	// Add session to manager
	if err := s.sessions.Add(session); err != nil {
		session.Close()
		msg := NewErrorMessage(err.Error())
		data, _ := msg.Marshal()
		conn.WriteMessage(websocket.TextMessage, data)
		return
	}

	// Handle the connection
	s.handleConnection(conn, session)
}

// handleConnection manages the bidirectional communication between WebSocket and PTY
func (s *Server) handleConnection(conn *websocket.Conn, session *Session) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer session.Close()

	// Set up ping/pong
	conn.SetPingHandler(func(appData string) error {
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
					cancel()
					return
				}
				if n > 0 {
					msg := NewOutputMessage(buf[:n])
					data, err := msg.Marshal()
					if err != nil {
						continue
					}
					if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
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
				return
			}

			msg, err := UnmarshalMessage(data)
			if err != nil {
				continue
			}

			if err := msg.Validate(); err != nil {
				errMsg := NewErrorMessage(err.Error())
				errData, _ := errMsg.Marshal()
				conn.WriteMessage(websocket.TextMessage, errData)
				continue
			}

			switch msg.Type {
			case MessageTypeInput:
				if _, err := session.Write(msg.Payload); err != nil {
					return
				}

			case MessageTypeResize:
				if err := session.Resize(msg.Cols, msg.Rows); err != nil {
					errMsg := NewErrorMessage(err.Error())
					errData, _ := errMsg.Marshal()
					conn.WriteMessage(websocket.TextMessage, errData)
				}

			case MessageTypePing:
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
				fmt.Printf("Closed %d idle terminal session(s)\n", closed)
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
