package deskstream

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Server serves the node desktop as an H.264 stream over a binary WebSocket
// (citadel-cli#338). It mirrors the VNC server's mesh exposure: it listens on
// localhost and on any tsnet VPN listeners attached via AddListener, with no
// application-layer auth (the tsnet mesh is the trust boundary). Input is
// unchanged and not handled here; this is VIDEO-ONLY.
//
// Each connection runs its OWN ffmpeg encoder so that a new viewer always
// starts at an IDR and an on-demand keyframe (requestKeyframe) can be served by
// restarting that viewer's encoder without disturbing others.
type Server struct {
	host string
	port int
	fps  int

	logger   Logger
	upgrader websocket.Upgrader

	mu             sync.Mutex
	running        bool
	httpServer     *http.Server
	extraListeners []net.Listener

	activeConns int64
	totalConns  int64
}

// Logger matches the VNC/terminal server logger interface.
type Logger interface {
	Printf(format string, v ...interface{})
}

type stdLogger struct{ l *log.Logger }

func (s *stdLogger) Printf(format string, v ...interface{}) { s.l.Printf(format, v...) }

type noOpLogger struct{}

func (n *noOpLogger) Printf(format string, v ...interface{}) {}

// Config configures the H.264 stream server.
type Config struct {
	Host string // bind host (default "127.0.0.1")
	Port int    // bind port (default DefaultPort)
	FPS  int    // target frame rate (default 15)
}

// DefaultPort is the default localhost/mesh port for the H.264 stream server.
// Chosen adjacent to the VNC port (5900) so node operators can reason about the
// desktop-related ports together; it is exposed over the tsnet mesh exactly
// like VNC.
const DefaultPort = 5910

// NewServer creates an H.264 stream server.
func NewServer(cfg Config) *Server {
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	if cfg.FPS <= 0 || cfg.FPS > 60 {
		cfg.FPS = 15
	}
	s := &Server{
		host:   cfg.Host,
		port:   cfg.Port,
		fps:    cfg.FPS,
		logger: &stdLogger{l: log.New(os.Stderr, "[h264] ", log.LstdFlags)},
	}
	s.upgrader = websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 64 * 1024,
		// Mesh-only exposure: like the VNC server, the tsnet listener is the
		// trust boundary, so we accept any origin (CLI/native clients send none).
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	return s
}

// SetSilent switches to a no-op logger (TUI mode).
func (s *Server) SetSilent() { s.logger = &noOpLogger{} }

// Port returns the configured port.
func (s *Server) Port() int { return s.port }

// ActiveConnections returns the number of active stream sessions.
func (s *Server) ActiveConnections() int64 { return atomic.LoadInt64(&s.activeConns) }

// AddListener registers an additional net.Listener (e.g. a tsnet VPN listener)
// that the server will also serve on. If the server is already running the
// listener begins serving immediately, so a VPN listener can be re-attached
// after a tsnet reconnect without restarting the server (issue #317).
func (s *Server) AddListener(ln net.Listener) {
	s.mu.Lock()
	s.extraListeners = append(s.extraListeners, ln)
	running := s.running
	httpServer := s.httpServer
	s.mu.Unlock()

	if running && httpServer != nil {
		s.logger.Printf("H.264 server also listening on %s (VPN, hot-attached)", ln.Addr())
		go func() {
			if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
				s.logger.Printf("VPN listener error: %v", err)
			}
		}()
	}
}

// RemoveListener drops a previously added extra listener from tracking so a
// long-lived session that re-attaches a VPN listener across reconnects does not
// accumulate dead references (issue #317). It does not close the listener.
func (s *Server) RemoveListener(ln net.Listener) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, l := range s.extraListeners {
		if l == ln {
			s.extraListeners = append(s.extraListeners[:i], s.extraListeners[i+1:]...)
			return
		}
	}
}

// Start begins accepting H.264 stream connections.
func (s *Server) Start() error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("H.264 server already running")
	}

	// Verify an encoder is available before claiming to be up, so a node without
	// ffmpeg/X reports a clear error and clients fall back to noVNC.
	if _, err := selectEncoder(ffmpegHasEncoder); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("H.264 unavailable: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/desktop/h264", s.handleStream)
	mux.HandleFunc("/health", s.handleHealth)

	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", s.host, s.port),
		Handler: mux,
		// No write timeout: a stream connection is long-lived. The per-message
		// write deadline is set on each frame inside the connection handler.
		ReadHeaderTimeout: 15 * time.Second,
	}

	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.running = false
		s.httpServer = nil
		s.mu.Unlock()
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	s.running = true
	extras := append([]net.Listener(nil), s.extraListeners...)
	httpServer := s.httpServer
	s.mu.Unlock()

	s.logger.Printf("H.264 server listening on %s (%d FPS)", ln.Addr(), s.fps)
	go func() {
		if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logger.Printf("server error: %v", err)
		}
	}()
	for _, extra := range extras {
		extra := extra
		s.logger.Printf("H.264 server also listening on %s (VPN)", extra.Addr())
		go func() {
			if err := httpServer.Serve(extra); err != nil && err != http.ErrServerClosed {
				s.logger.Printf("VPN listener error: %v", err)
			}
		}()
	}
	return nil
}

// Stop gracefully shuts down the server.
func (s *Server) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	httpServer := s.httpServer
	s.httpServer = nil
	s.mu.Unlock()

	if httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}
	s.logger.Printf("H.264 server stopped (total=%d)", atomic.LoadInt64(&s.totalConns))
}

// IsRunning reports whether the server is running.
func (s *Server) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok","codec":"h264","active":%d}`, atomic.LoadInt64(&s.activeConns))
}
