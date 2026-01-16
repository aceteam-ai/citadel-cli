package status

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Server provides an HTTP server for node status queries.
// This enables on-demand queries from the AceTeam control plane.
type Server struct {
	collector  *Collector
	port       int
	httpServer *http.Server
	version    string
}

// ServerConfig holds configuration for the status server.
type ServerConfig struct {
	Port    int    // HTTP server port (default: 8080)
	Version string // Citadel version string
}

// NewServer creates a new status HTTP server.
func NewServer(cfg ServerConfig, collector *Collector) *Server {
	if cfg.Port == 0 {
		cfg.Port = 8080
	}
	return &Server{
		collector: collector,
		port:      cfg.Port,
		version:   cfg.Version,
	}
}

// Start begins listening for HTTP requests.
// This method blocks until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/services", s.handleServices)

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
