package cobrowsestream

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

const (
	// writeWait is the deadline for a single WebSocket write to the viewer.
	writeWait = 10 * time.Second
	// pongWait / pingPeriod keep the viewer connection alive and, crucially,
	// detect a dead viewer promptly so the session flips back out of "attached".
	pongWait   = 60 * time.Second
	pingPeriod = 25 * time.Second
	// viewerReadLimit bounds an input frame; CDP input JSON is tiny.
	viewerReadLimit = 8 * 1024
)

// Session is the slice of the co-browse session manager this server needs. It is
// an interface so the streaming server is unit-testable without a real browser
// (a fake Session + a fake CDP endpoint), and so the manager (#793) needs no
// edits -- NewManagerSession adapts the existing exported API.
type Session interface {
	// AttachTarget returns the CDP debug port for a session that is attachable
	// right now (running, with a live debug port). ok is false for an unknown,
	// still-launching, or exited session.
	AttachTarget(id string) (debugPort int, ok bool)
	// MarkAttached flips the session to attached when a viewer connects.
	MarkAttached(id string) bool
	// MarkDetached returns the session to running when the viewer disconnects.
	MarkDetached(id string) bool
}

// Logger matches the deskstream/VNC/terminal server logger interface.
type Logger interface {
	Printf(format string, v ...interface{})
}

type stdLogger struct{ l *log.Logger }

func (s *stdLogger) Printf(format string, v ...interface{}) { s.l.Printf(format, v...) }

type noOpLogger struct{}

func (n *noOpLogger) Printf(format string, v ...interface{}) {}

// Config configures the co-browse stream server.
type Config struct {
	Host      string // bind host (default "127.0.0.1")
	Port      int    // bind port (default services.CobrowseStreamPort, see cmd wiring)
	Quality   int    // JPEG quality 1-100 (default 60)
	MaxWidth  int    // screencast max width in px (default 1280)
	MaxHeight int    // screencast max height in px (default 720)
	EveryNth  int    // capture every Nth frame at the source (default 1)
}

func (c *Config) applyDefaults() {
	if c.Host == "" {
		c.Host = "127.0.0.1"
	}
	if c.Quality <= 0 || c.Quality > 100 {
		c.Quality = 60
	}
	if c.MaxWidth <= 0 {
		c.MaxWidth = 1280
	}
	if c.MaxHeight <= 0 {
		c.MaxHeight = 720
	}
	if c.EveryNth <= 0 {
		c.EveryNth = 1
	}
}

// Server screencasts one co-browse session to a viewer and forwards the viewer's
// input back, over a binary+text WebSocket. It mirrors the deskstream server's
// mesh exposure: it listens on localhost and on any tsnet VPN listeners attached
// via AddListener, with no application-layer auth (the tsnet mesh is the trust
// boundary). Exactly ONE viewer may stream a given session at a time, so input is
// never ambiguous between two viewers.
type Server struct {
	cfg     Config
	session Session

	logger   Logger
	upgrader websocket.Upgrader

	mu             sync.Mutex
	running        bool
	httpServer     *http.Server
	boundAddr      string
	extraListeners []net.Listener
	// active maps a session id to the live viewer's claim so a second viewer of
	// the same session is refused, and so Stop can proactively drop live viewers.
	active map[string]*viewerClaim

	activeConns int64
	totalConns  int64
}

// NewServer creates a co-browse stream server bound to the given session manager.
func NewServer(cfg Config, session Session) *Server {
	cfg.applyDefaults()
	s := &Server{
		cfg:     cfg,
		session: session,
		logger:  &stdLogger{l: log.New(os.Stderr, "[cobrowse-stream] ", log.LstdFlags)},
		active:  make(map[string]*viewerClaim),
	}
	s.upgrader = websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 64 * 1024,
		// Mesh-only exposure: the tsnet listener is the trust boundary, so accept
		// any origin (CLI/native clients send none).
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	return s
}

// SetSilent switches to a no-op logger (TUI mode).
func (s *Server) SetSilent() { s.logger = &noOpLogger{} }

// Port returns the configured port.
func (s *Server) Port() int { return s.cfg.Port }

// BoundAddr returns the primary localhost listener's actual address once Start
// has run (useful when Port is 0 for an ephemeral bind), or "" before Start.
func (s *Server) BoundAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.boundAddr
}

// ActiveConnections returns the number of live viewer sessions.
func (s *Server) ActiveConnections() int64 { return atomic.LoadInt64(&s.activeConns) }

// AddListener registers an additional net.Listener (e.g. a tsnet VPN listener)
// the server also serves on. If already running it begins serving immediately,
// so a VPN listener can be re-attached after a tsnet reconnect without a restart
// (mirrors deskstream #317).
func (s *Server) AddListener(ln net.Listener) {
	s.mu.Lock()
	s.extraListeners = append(s.extraListeners, ln)
	running := s.running
	httpServer := s.httpServer
	s.mu.Unlock()

	if running && httpServer != nil {
		s.logger.Printf("also listening on %s (VPN, hot-attached)", ln.Addr())
		go func() {
			if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
				s.logger.Printf("VPN listener error: %v", err)
			}
		}()
	}
}

// RemoveListener drops a previously added extra listener from tracking so a
// session that re-attaches a VPN listener across reconnects does not accumulate
// dead references. It does not close the listener.
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

// Start begins accepting viewer connections on localhost plus any extra listeners.
func (s *Server) Start() error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("cobrowse stream server already running")
	}

	mux := http.NewServeMux()
	mux.HandleFunc(StreamPath, s.handleStream)
	mux.HandleFunc(HealthPath, s.handleHealth)

	s.httpServer = &http.Server{
		Handler: mux,
		// No write timeout: a stream connection is long-lived. Per-message write
		// deadlines are set on each frame inside the handler.
		ReadHeaderTimeout: 15 * time.Second,
	}

	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.httpServer = nil
		s.mu.Unlock()
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	s.running = true
	s.boundAddr = ln.Addr().String()
	extras := append([]net.Listener(nil), s.extraListeners...)
	httpServer := s.httpServer
	s.mu.Unlock()

	s.logger.Printf("listening on %s", ln.Addr())
	go func() {
		if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logger.Printf("server error: %v", err)
		}
	}()
	for _, extra := range extras {
		extra := extra
		s.logger.Printf("also listening on %s (VPN)", extra.Addr())
		go func() {
			if err := httpServer.Serve(extra); err != nil && err != http.ErrServerClosed {
				s.logger.Printf("VPN listener error: %v", err)
			}
		}()
	}
	return nil
}

// Stop gracefully shuts the server down, proactively cancelling any live viewer
// so its CDP attachment is torn down (no leak) before the HTTP server closes.
func (s *Server) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	httpServer := s.httpServer
	s.httpServer = nil
	cancels := make([]context.CancelFunc, 0, len(s.active))
	for _, vc := range s.active {
		cancels = append(cancels, vc.cancel)
	}
	s.mu.Unlock()

	for _, c := range cancels {
		c()
	}
	if httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}
	s.logger.Printf("stopped (total=%d)", atomic.LoadInt64(&s.totalConns))
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
	fmt.Fprintf(w, `{"status":"ok","active":%d}`, atomic.LoadInt64(&s.activeConns))
}

// viewerClaim is the identity token for one live viewer of a session, held in
// the active map so release can drop only its OWN claim.
type viewerClaim struct{ cancel context.CancelFunc }

// claim registers a viewer for a session id, refusing a second viewer of the
// same session. Returns nil, false if the session is already being streamed.
func (s *Server) claim(id string, cancel context.CancelFunc) (*viewerClaim, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.active[id]; exists {
		return nil, false
	}
	vc := &viewerClaim{cancel: cancel}
	s.active[id] = vc
	return vc, true
}

// release drops a session's claim, but only if it still points at THIS viewer's
// claim -- so a viewer tearing down cannot evict a different live claim.
func (s *Server) release(id string, vc *viewerClaim) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.active[id]; ok && cur == vc {
		delete(s.active, id)
	}
}
