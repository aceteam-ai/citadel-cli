package status

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/desktop"
	"github.com/aceteam-ai/citadel-cli/internal/terminal"
)

// RouteRegistrar is a callback that registers HTTP routes on the status server's
// mux with the given auth middleware. This avoids circular imports between the
// status package and feature packages (e.g., provision).
type RouteRegistrar func(mux *http.ServeMux, authMiddleware func(http.HandlerFunc) http.HandlerFunc)

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

	// gatewayCertPath is the on-disk path to the gateway's self-signed leaf cert
	// PEM. When set, the status server serves it unauthenticated at
	// GET /gateway-cert.pem so the backend can fetch (and re-fetch on rotation)
	// the cert it must trust to reach this node's TLS gateway. Empty means the
	// gateway runs without TLS (--gateway-no-tls), in which case the endpoint
	// returns 204 No Content to signal "use plain http".
	gatewayCertPath string

	// agent provides the data/actions backing the /agent/* introspection &
	// control endpoints (issue #236). Nil disables those routes.
	agent *AgentProviders

	// routeRegistrars are callbacks registered via AddRouteRegistrar that
	// install additional routes (e.g., provisioning API) during Start.
	routeRegistrars []RouteRegistrar

	// extraRoutes is called during Start() to register additional HTTP routes.
	extraRoutes func(mux *http.ServeMux)
	// extraListeners are additional net.Listeners the server will also serve on
	// (e.g., a tsnet VPN listener). Added via AddListener before Start.
	extraListeners []net.Listener

	// caVerifier, when set, gates the MUTATING control endpoints (SSH-key
	// injection) behind a fabric-CA-signed coordinator client certificate over
	// mTLS (issue #5028). Nil means the mutating endpoints are refused on every
	// listener (fail closed) -- they are NEVER served over the plaintext,
	// VPN-origin-trusting path anymore.
	caVerifier *FabricCAVerifier
	// controlPort is the port for the dedicated mTLS control listener that serves
	// the mutating endpoints. Only used when caVerifier and controlServerCert are
	// both set.
	controlPort int
	// controlServerCert is the TLS server certificate the control listener
	// presents. The caller (coordinator) authenticates the node out-of-band (mesh
	// + gateway cert), so a self-signed server cert is sufficient here -- the
	// load-bearing check is the CLIENT cert the node verifies.
	controlServerCert *tls.Certificate
	// controlHTTPServer is the running control server (set during Start) so
	// shutdown can close it alongside the plaintext server.
	controlHTTPServer *http.Server
	// extraControlListeners are additional net.Listeners (e.g., a tsnet VPN
	// listener) the mTLS control server will also serve on. Added via
	// AddControlListener before Start.
	extraControlListeners []net.Listener
}

// ServerConfig holds configuration for the status server.
type ServerConfig struct {
	Port           int                     // HTTP server port (default: 8080)
	Version        string                  // Citadel version string
	TokenValidator terminal.TokenValidator // Optional: enables authenticated desktop endpoints
	OrgID          string                  // Required when TokenValidator is set
	EnableDesktop  bool                    // When true AND TokenValidator is set, registers /api/screenshot and /api/actions

	// Agent, when set, registers the /agent/* introspection & control
	// endpoints (issue #236). These are served over the same dual (LAN+VPN)
	// listeners but gated by requireVPNOrAuth.
	Agent *AgentProviders

	// ExtraRoutes, when set, is called during Start() to register additional
	// HTTP routes on the status server's mux. This allows external packages
	// (e.g., workflow) to add endpoints without modifying the status package.
	ExtraRoutes func(mux *http.ServeMux)

	// GatewayCertPath is the on-disk path to the gateway's self-signed leaf cert
	// PEM. When non-empty, GET /gateway-cert.pem serves that PEM unauthenticated
	// (a public leaf cert is safe to hand out) so the backend can fetch the cert
	// it must trust to reach this node's TLS gateway, and re-fetch it on rotation.
	// Empty means the gateway has no TLS cert (--gateway-no-tls); the endpoint
	// then returns 204 No Content to tell the backend to use plain http.
	GatewayCertPath string

	// CAVerifier, when set, enables the dedicated mTLS control listener that
	// serves the MUTATING control endpoints (SSH-key injection) gated by a
	// fabric-CA-signed coordinator client certificate (issue #5028). When nil,
	// those endpoints are refused everywhere (fail closed).
	CAVerifier *FabricCAVerifier
	// ControlPort is the port for the mTLS control listener (default: 8443). Only
	// used when CAVerifier and ControlServerCert are both set.
	ControlPort int
	// ControlServerCert is the TLS server certificate the control listener
	// presents. Required (together with CAVerifier) to start the control listener.
	ControlServerCert *tls.Certificate
}

// DefaultControlPort is the default port for the mTLS control listener.
const DefaultControlPort = 8443

// NewServer creates a new status HTTP server.
func NewServer(cfg ServerConfig, collector *Collector) *Server {
	if cfg.Port == 0 {
		cfg.Port = 8080
	}
	if cfg.ControlPort == 0 {
		cfg.ControlPort = DefaultControlPort
	}
	return &Server{
		collector:         collector,
		port:              cfg.Port,
		version:           cfg.Version,
		tokenValidator:    cfg.TokenValidator,
		orgID:             cfg.OrgID,
		enableDesktop:     cfg.EnableDesktop,
		agent:             cfg.Agent,
		extraRoutes:       cfg.ExtraRoutes,
		gatewayCertPath:   cfg.GatewayCertPath,
		caVerifier:        cfg.CAVerifier,
		controlPort:       cfg.ControlPort,
		controlServerCert: cfg.ControlServerCert,
	}
}

// AddListener registers an additional net.Listener that the server will also
// serve on when Start is called. This enables dual-listen on both LAN and VPN
// interfaces. Must be called before Start.
func (s *Server) AddListener(ln net.Listener) {
	s.extraListeners = append(s.extraListeners, ln)
}

// AddControlListener registers an additional net.Listener that the mTLS control
// server will also serve on (e.g., a tsnet VPN listener so the coordinator can
// reach the control endpoints over the mesh). The listener is TLS-wrapped by the
// control server; pass a raw TCP/tsnet listener. Must be called before Start.
func (s *Server) AddControlListener(ln net.Listener) {
	s.extraControlListeners = append(s.extraControlListeners, ln)
}

// controlEnabled reports whether the mTLS control listener can be started: it
// requires both a fabric CA verifier (to authenticate/authorize callers) and a
// server certificate (to present on the TLS handshake). Absent either, the
// mutating control endpoints are refused everywhere (fail closed).
func (s *Server) controlEnabled() bool {
	return s.caVerifier != nil && s.controlServerCert != nil
}

// handleSSHMovedToControl is the fail-closed stub left on the PLAINTEXT status
// listener where /ssh/authorized-keys used to be served under requireVPNOrAuth.
// It never writes keys. It returns 403 with a pointer to the mTLS control
// listener so a legitimate coordinator discovers the new requirement, while a
// cross-org mesh caller is simply denied.
func (s *Server) handleSSHMovedToControl(w http.ResponseWriter, r *http.Request) {
	msg := "SSH key deployment requires a coordinator mTLS client certificate on the control listener"
	if s.controlEnabled() {
		msg = fmt.Sprintf("%s (port %d)", msg, s.controlPort)
	}
	writeJSONError(w, msg, http.StatusForbidden)
}

// buildControlMux constructs the multiplexer for the dedicated mTLS control
// listener. Only MUTATING control endpoints live here, each gated by
// requireCoordinator (a verified fabric-CA-signed coordinator client cert). The
// caller must ensure s.controlEnabled() is true before serving this mux.
func (s *Server) buildControlMux() *http.ServeMux {
	mux := http.NewServeMux()
	// Liveness probe (still requires the mTLS handshake to reach, but no identity
	// check) so a coordinator can confirm the control listener is up.
	mux.HandleFunc("/ping", s.handlePing)
	// SSH-key injection: the host-takeover crown jewel. Coordinator identity only.
	mux.HandleFunc("/ssh/authorized-keys", s.caVerifier.requireCoordinator(s.handleSSHAuthorizedKeys))
	return mux
}

// AddRouteRegistrar registers a callback that will be invoked during Start to
// install additional HTTP routes on the server's mux. The callback receives
// the mux and the requireVPNOrAuth middleware for auth gating.
// Must be called before Start.
func (s *Server) AddRouteRegistrar(reg RouteRegistrar) {
	s.routeRegistrars = append(s.routeRegistrars, reg)
}

// buildMux constructs the HTTP route multiplexer for the server, registering
// all enabled endpoints based on the server's configuration. It reads config
// fields but mutates no shared Server state, so it can be invoked synchronously
// from both Start and tests without introducing data races.
func (s *Server) buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", s.handlePing)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/services", s.handleServices)

	// Publish the gateway's self-signed leaf cert so the backend can trust it
	// out-of-band (and re-fetch on rotation). Served unauthenticated over the
	// plaintext status server so it is a bootstrap channel that does not require
	// already trusting the cert being fetched. A public leaf cert is safe to hand
	// out. Returns 204 when the gateway runs without TLS.
	mux.HandleFunc("/gateway-cert.pem", s.handleGatewayCert)

	if s.tokenValidator != nil && s.enableDesktop {
		mux.HandleFunc("/api/screenshot", s.requireAuth(s.handleScreenshot))
		mux.HandleFunc("/api/actions", s.requireAuth(s.handleActions))
	}

	// SSH key deployment is a MUTATING, host-takeover-capable endpoint. It is no
	// longer served over this plaintext, VPN-origin-trusting listener (issue
	// #5028): mesh origin is not an identity, so any node on the flat mesh could
	// inject a key. It now lives ONLY on the dedicated mTLS control listener,
	// gated by a coordinator client certificate (see buildControlMux). Here we
	// register a fail-closed stub so the old path refuses with a clear signal
	// instead of silently 404ing or, worse, still writing keys.
	mux.HandleFunc("/ssh/authorized-keys", s.handleSSHMovedToControl)

	// Agent introspection & control endpoints (issue #236), gated the same way
	// (VPN origin or valid org token). No-op when no providers were supplied.
	s.registerAgentRoutes(mux)

	// Invoke registered route registrars (e.g., provisioning API).
	for _, reg := range s.routeRegistrars {
		reg(mux, s.requireVPNOrAuth)
	}

	// Allow external packages to register extra routes (e.g., workflow API).
	if s.extraRoutes != nil {
		s.extraRoutes(mux)
	}

	return mux
}

// Start begins listening for HTTP requests.
// This method blocks until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	mux := s.buildMux()
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

	// Start the dedicated mTLS control listener for mutating endpoints (#5028).
	// When it is not enabled, the mutating endpoints stay refused everywhere
	// (fail closed) via the plaintext stub.
	s.startControlServer()

	// Wait for context cancellation or server error
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if s.controlHTTPServer != nil {
			_ = s.controlHTTPServer.Shutdown(shutdownCtx)
		}
		return s.httpServer.Shutdown(shutdownCtx)
	case err := <-errChan:
		return err
	}
}

// startControlServer starts the mTLS control listener (issue #5028) when a fabric
// CA verifier and server certificate are configured. The listener requires a
// coordinator client certificate (RequireAndVerifyClientCert against the fabric
// CA) at the TLS handshake; mutating handlers additionally verify the coordinator
// SAN identity. Serves on a local TCP port plus any listeners added via
// AddControlListener (e.g., the tsnet VPN listener). No-op (fail closed) when the
// control listener is not enabled.
func (s *Server) startControlServer() {
	if !s.controlEnabled() {
		log.Printf("[status] mTLS control listener DISABLED: SSH-key injection and other " +
			"mutating control endpoints are refused (no fabric CA verifier / server cert configured)")
		return
	}

	tlsConfig := s.caVerifier.ServerTLSConfig(*s.controlServerCert)
	s.controlHTTPServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.controlPort),
		Handler:      s.buildControlMux(),
		TLSConfig:    tlsConfig,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	// Local TCP listener, TLS-wrapped. ServeTLS with empty cert/key paths uses the
	// certificate already set in TLSConfig. A failure here (e.g. the control port
	// is occupied) must NOT tear down the read-only status server -- it only means
	// the mutating endpoints stay refused (fail closed), so we log and move on
	// rather than propagating to the status server's fatal error path.
	go func() {
		log.Printf("[status] mTLS control listener on :%d (coordinator client cert required)", s.controlPort)
		if err := s.controlHTTPServer.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Printf("[status] mTLS control listener error (mutating endpoints refused): %v", err)
		}
	}()

	// Serve the same mTLS control server on any extra (e.g., VPN) listeners.
	for _, ln := range s.extraControlListeners {
		ln := ln // capture loop variable
		tlsLn := tls.NewListener(ln, tlsConfig)
		log.Printf("[status] mTLS control listener also on %s (VPN)", ln.Addr().String())
		go func() {
			if err := s.controlHTTPServer.Serve(tlsLn); err != nil && err != http.ErrServerClosed {
				log.Printf("[status] control VPN listener error: %v", err)
			}
		}()
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

// handleGatewayCert serves the gateway's current self-signed leaf cert PEM so the
// backend can trust it out-of-band and re-fetch it after a rotation.
// GET /gateway-cert.pem
//
// Unauthenticated by design: the response is a PUBLIC leaf cert (safe to hand
// out), and this is the bootstrap/refresh channel the backend uses BEFORE it
// trusts the cert, so it must not require the very trust it establishes. It reads
// the current PEM from disk on every request so a rotated cert (the gateway
// regenerates it in place when the mesh IP changes) is always served fresh.
//
// Returns 204 No Content when the gateway has no TLS cert (--gateway-no-tls),
// signaling the backend to reach the node over plain http instead. Returns 503
// when TLS is configured but the cert is not yet on disk (a cold-start race) so
// the backend retries rather than mis-downgrading to http.
func (s *Server) handleGatewayCert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.gatewayCertPath == "" {
		// No TLS cert configured: the gateway runs --gateway-no-tls. Tell the
		// backend to use plain http.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	pem, err := os.ReadFile(s.gatewayCertPath)
	if err != nil {
		// TLS IS configured (path is set) but the cert is not on disk yet -- a
		// brief cold-start race before the gateway writes it. Do NOT return 204
		// here: 204 means "TLS off, use plain http", and downgrading to http
		// against a TLS gateway would fail. Return 503 so the backend RETRIES
		// (from cert_refresh_url) once the cert exists, instead of mis-detecting
		// the node as no-TLS.
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.WriteHeader(http.StatusOK)
	w.Write(pem)
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

	// Per-request readiness check: verify the desktop environment is usable
	// before attempting capture. Returns 503 with actionable diagnostics when
	// the display or screenshot tools are unavailable.
	if err := desktop.DiagnoseDesktopReadiness(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		if re, ok := err.(*desktop.VNCReadinessError); ok {
			json.NewEncoder(w).Encode(map[string]string{
				"error":  re.Error(),
				"reason": re.Reason,
				"detail": re.Detail,
			})
		} else {
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		}
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

	// Per-request readiness check: verify the desktop environment is usable
	// before attempting input actions.
	if err := desktop.DiagnoseDesktopReadiness(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		if re, ok := err.(*desktop.VNCReadinessError); ok {
			json.NewEncoder(w).Encode(map[string]string{
				"error":  re.Error(),
				"reason": re.Reason,
				"detail": re.Detail,
			})
		} else {
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		}
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
