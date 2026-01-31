// Package fabricserver provides an embedded HTTP server for receiving direct
// inter-node calls via the Headscale VPN mesh.
//
// The server listens only on the VPN interface (100.64.x.x) and exposes
// health check, service proxy, and shell exec endpoints. It is used for
// "specific resource" routing where workloads must target a particular node.
package fabricserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ServiceHandler is a function that handles a direct fabric call.
type ServiceHandler func(w http.ResponseWriter, r *http.Request)

// Server is the embedded HTTP server for inter-node communication.
type Server struct {
	config   Config
	mux      *http.ServeMux
	server   *http.Server
	mu       sync.RWMutex
	services map[string]ServiceHandler
}

// Config holds configuration for the fabric server.
type Config struct {
	// Port to listen on (default: 8443)
	Port int

	// ListenAddress overrides the listen address.
	// If empty, auto-detects the VPN interface (100.64.x.x).
	ListenAddress string

	// NodeName is the name of this node (for logging/headers).
	NodeName string

	// ReadTimeout is the max time to read a request (default: 30s).
	ReadTimeout time.Duration

	// WriteTimeout is the max time to write a response (default: 60s).
	WriteTimeout time.Duration
}

// NewServer creates a new fabric server.
func NewServer(cfg Config) *Server {
	if cfg.Port == 0 {
		cfg.Port = 8443
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 30 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 60 * time.Second
	}

	s := &Server{
		config:   cfg,
		mux:      http.NewServeMux(),
		services: make(map[string]ServiceHandler),
	}

	// Register built-in routes
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/api/services", s.handleListServices)
	s.mux.HandleFunc("/api/", s.handleServiceProxy)

	return s
}

// RegisterService registers a named service handler.
// The service will be accessible at /api/{serviceName}/...
func (s *Server) RegisterService(name string, handler ServiceHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.services[name] = handler
}

// Start begins listening. Blocks until context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	listenAddr := s.config.ListenAddress
	if listenAddr == "" {
		vpnIP, err := detectVPNAddress()
		if err != nil {
			// Fall back to localhost if no VPN interface found
			log.Printf("[FabricServer] No VPN interface found (%v), listening on localhost", err)
			listenAddr = fmt.Sprintf("127.0.0.1:%d", s.config.Port)
		} else {
			listenAddr = fmt.Sprintf("%s:%d", vpnIP, s.config.Port)
		}
	}

	s.server = &http.Server{
		Addr:         listenAddr,
		Handler:      s.loggingMiddleware(s.mux),
		ReadTimeout:  s.config.ReadTimeout,
		WriteTimeout: s.config.WriteTimeout,
	}

	fmt.Printf("   - Fabric server: http://%s\n", listenAddr)

	// Start listening
	errCh := make(chan error, 1)
	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	// Wait for context cancellation or error
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.server.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// handleHealth returns basic health information.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	serviceCount := len(s.services)
	s.mu.RUnlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   "ok",
		"node":     s.config.NodeName,
		"services": serviceCount,
		"time":     time.Now().UTC().Format(time.RFC3339),
	})
}

// handleListServices returns registered services.
func (s *Server) handleListServices(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	names := make([]string, 0, len(s.services))
	for name := range s.services {
		names = append(names, name)
	}
	s.mu.RUnlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"node":     s.config.NodeName,
		"services": names,
	})
}

// handleServiceProxy routes /api/{service}/... to the registered handler.
func (s *Server) handleServiceProxy(w http.ResponseWriter, r *http.Request) {
	// Extract service name from path: /api/{service}/...
	path := strings.TrimPrefix(r.URL.Path, "/api/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, `{"error":"service name required"}`, http.StatusBadRequest)
		return
	}

	serviceName := parts[0]

	s.mu.RLock()
	handler, exists := s.services[serviceName]
	s.mu.RUnlock()

	if !exists {
		http.Error(w, fmt.Sprintf(`{"error":"service %q not found"}`, serviceName), http.StatusNotFound)
		return
	}

	handler(w, r)
}

// loggingMiddleware logs all requests for audit trail.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		source := r.Header.Get("X-Fabric-Source")
		if source == "" {
			source = r.RemoteAddr
		}

		next.ServeHTTP(w, r)

		log.Printf("[FabricServer] %s %s from %s (%s)",
			r.Method, r.URL.Path, source, time.Since(start).Round(time.Millisecond))
	})
}

// detectVPNAddress finds the Headscale VPN interface IP (100.64.x.x).
func detectVPNAddress() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", fmt.Errorf("failed to list interfaces: %w", err)
	}

	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP.String()
		if strings.HasPrefix(ip, "100.64.") {
			return ip, nil
		}
	}

	return "", fmt.Errorf("no VPN interface (100.64.x.x) found")
}
