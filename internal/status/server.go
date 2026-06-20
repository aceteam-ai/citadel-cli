package status

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/desktop"
	"github.com/aceteam-ai/citadel-cli/internal/terminal"
)

// Server provides an HTTP server for node status queries.
// This enables on-demand queries from the AceTeam control plane.
type Server struct {
	collector      *Collector
	port           int
	httpServer     *http.Server
	version        string
	tokenValidator terminal.TokenValidator
	orgID          string
	enableDesktop  bool

	// agent provides the data/actions backing the /agent/* introspection &
	// control endpoints (issue #236). Nil disables those routes.
	agent *AgentProviders

	// extraListeners are additional net.Listeners the server will also serve on
	// (e.g., a tsnet VPN listener). Added via AddListener before Start.
	extraListeners []net.Listener
}

// ServerConfig holds configuration for the status server.
type ServerConfig struct {
	Port           int                    // HTTP server port (default: 8080)
	Version        string                 // Citadel version string
	TokenValidator terminal.TokenValidator // Optional: enables authenticated desktop endpoints
	OrgID          string                 // Required when TokenValidator is set
	EnableDesktop  bool                   // When true AND TokenValidator is set, registers /api/screenshot and /api/actions

	// Agent, when set, registers the /agent/* introspection & control
	// endpoints (issue #236). These are served over the same dual (LAN+VPN)
	// listeners but gated by requireVPNOrAuth.
	Agent *AgentProviders
}

// NewServer creates a new status HTTP server.
func NewServer(cfg ServerConfig, collector *Collector) *Server {
	if cfg.Port == 0 {
		cfg.Port = 8080
	}
	return &Server{
		collector:      collector,
		port:           cfg.Port,
		version:        cfg.Version,
		tokenValidator: cfg.TokenValidator,
		orgID:          cfg.OrgID,
		enableDesktop:  cfg.EnableDesktop,
		agent:          cfg.Agent,
	}
}

// AddListener registers an additional net.Listener that the server will also
// serve on when Start is called. This enables dual-listen on both LAN and VPN
// interfaces. Must be called before Start.
func (s *Server) AddListener(ln net.Listener) {
	s.extraListeners = append(s.extraListeners, ln)
}

// Start begins listening for HTTP requests.
// This method blocks until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", s.handlePing)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/services", s.handleServices)

	if s.tokenValidator != nil && s.enableDesktop {
		mux.HandleFunc("/api/screenshot", s.requireAuth(s.handleScreenshot))
		mux.HandleFunc("/api/actions", s.requireAuth(s.handleActions))
	}

	// SSH key deployment: available on all nodes (headless or with desktop).
	// Uses VPN-origin check OR token auth — the platform relay calls this
	// from within the VPN mesh after validating org ownership.
	mux.HandleFunc("/ssh/authorized-keys", s.requireVPNOrAuth(s.handleSSHAuthorizedKeys))

	// Agent introspection & control endpoints (issue #236), gated the same way
	// (VPN origin or valid org token). No-op when no providers were supplied.
	s.registerAgentRoutes(mux)

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.port),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	// Start server in goroutine
	errChan := make(chan error, 1)
	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errChan <- err
		}
	}()

	// Serve on any extra listeners (e.g., tsnet VPN)
	for _, ln := range s.extraListeners {
		ln := ln // capture loop variable
		log.Printf("[status] also listening on %s (VPN)", ln.Addr().String())
		go func() {
			if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
				log.Printf("[status] VPN listener error: %v", err)
			}
		}()
	}

	// Wait for context cancellation or server error
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.httpServer.Shutdown(shutdownCtx)
	case err := <-errChan:
		return err
	}
}

// Port returns the port the server is configured to listen on.
func (s *Server) Port() int {
	return s.port
}

// handleStatus returns the full node status.
// GET /status
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	status, err := s.collector.Collect()
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to collect status: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// handleHealth returns a simple health check response.
// GET /health
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Determine overall health based on collector status
	healthStatus := HealthStatusOK

	// Could add more sophisticated health checking here:
	// - Check if critical services are running
	// - Check if GPU is overheating
	// - Check if disk is nearly full

	resp := HealthResponse{
		Status:  healthStatus,
		Version: s.version,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleServices returns only the services section of the status.
// GET /services
func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	status, err := s.collector.Collect()
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to collect status: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"services": status.Services,
	})
}

// handlePing returns a lightweight pong response for health checks.
// GET /ping
// This is useful since ICMP ping doesn't work with userspace networking.
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "pong",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || token == r.Header.Get("Authorization") {
			http.Error(w, `{"error":"authorization required"}`, http.StatusUnauthorized)
			return
		}
		if _, err := s.tokenValidator.ValidateToken(token, s.orgID); err != nil {
			http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// handleScreenshot captures and returns a PNG screenshot of the display.
// GET /api/screenshot
func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	png, err := desktop.CaptureScreenshot(r.Context())
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(png)
}

// handleActions executes mouse/keyboard actions on the display.
// POST /api/actions
func (s *Server) handleActions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to read body"})
		return
	}
	actions, err := desktop.ParseActions(body)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if err := desktop.ExecuteActions(r.Context(), actions); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":      true,
		"actions": len(actions),
	})
}
