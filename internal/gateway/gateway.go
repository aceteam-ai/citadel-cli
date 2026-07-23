// Package gateway provides an HTTPS reverse proxy that consolidates all Citadel
// node services behind a single TLS endpoint. It routes requests to the
// appropriate backend based on URL path prefix.
//
// Route table:
//
//	/health           -> status server /health
//	/status           -> status server /status
//	/ping             -> status server /ping
//	/services         -> status server /services
//	/api/screenshot   -> status server /api/screenshot
//	/api/actions      -> status server /api/actions
//	/api/...          -> fabric server /api/...
//	/vnc              -> VNC WebSocket proxy (requires websockify)
//	/terminal         -> terminal WebSocket server
package gateway

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

// ModuleRoutePrefix is the namespace under which EVERY provisioned module is
// exposed on the mesh: the gateway serves a module declaring gateway.prefix=<p>
// at /modules/<p>/ and strips the /modules/<p> prefix before forwarding to the
// module's loopback port, so the module's own paths (/health, /qr.txt, ...) map
// through unchanged. Namespacing under /modules/ prevents collision with builtin
// routes (/terminal, /vnc, /provision, ...) and between modules. Which prefixes
// exist, at which port, and under which capability is DATA in the
// provisioned-service registry -- not code here.
const ModuleRoutePrefix = "/modules/"

// ModuleRoutePath returns the gateway route path for a module prefix:
// "/modules/<prefix>". Used by both the gateway (route registration) and the
// out-of-process URL builder so the convention lives in one place.
func ModuleRoutePath(prefix string) string {
	return ModuleRoutePrefix + prefix
}

// CapabilityResolver reports which capability gates the module served under the
// given gateway prefix (the <p> in /modules/<p>/...), and whether such a module
// is registered. The gateway holds one (backed by the provisioned-service
// registry) so categoryForPath can gate module routes from declared data instead
// of a hardcoded switch. A nil resolver (or unknown prefix) falls back to the
// provision capability, so a module route is never accidentally left ungated.
type CapabilityResolver interface {
	CapabilityForPrefix(prefix string) (capability string, found bool)
}

// Config holds configuration for the gateway server.
type Config struct {
	// Port is the HTTPS port to listen on (default: 8443).
	Port int

	// ListenAddress overrides the listen address.
	// If empty, listens on all interfaces (0.0.0.0).
	ListenAddress string

	// TLSConfig is the TLS configuration with certificates.
	TLSConfig *tls.Config

	// NodeName is used for logging and response headers.
	NodeName string

	// Upstreams maps path prefixes to backend addresses.
	// Populated via AddUpstream before Start.
	Upstreams map[string]*Upstream
}

// Upstream describes a backend service to proxy to.
type Upstream struct {
	// Address is the backend address (e.g., "127.0.0.1:8080").
	//
	// For most upstreams the address is fixed at registration time. For a
	// dynamically-provisioned module (which binds an auto-selected free host port
	// AFTER the gateway has already started), the
	// address is not known when the route is registered. Such a route is
	// registered up front with an empty Address and its target is set later via
	// Server.SetUpstreamAddress; reads go through addr() so a live proxy always
	// forwards to the current target. Direct field reads are avoided in the proxy
	// hot path for exactly that reason.
	Address string

	// StripPrefix removes the matched prefix before forwarding.
	// For example, if the gateway matches "/vnc" and StripPrefix is true,
	// a request to "/vnc/foo" is forwarded as "/foo".
	StripPrefix bool

	// WebSocket indicates this upstream handles WebSocket connections.
	WebSocket bool

	// mu guards dynAddr for upstreams whose target is set after registration.
	mu sync.RWMutex
	// dynAddr, when non-empty, overrides Address. It is set via
	// Server.SetUpstreamAddress for dynamically-provisioned upstreams.
	dynAddr string
}

// addr returns the current backend address, preferring a dynamically-set
// address over the static one. Safe for concurrent use.
func (u *Upstream) addr() string {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if u.dynAddr != "" {
		return u.dynAddr
	}
	return u.Address
}

// setAddr updates the dynamic backend address. Safe for concurrent use.
func (u *Upstream) setAddr(a string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.dynAddr = a
}

// Server is the HTTPS reverse proxy gateway.
type Server struct {
	config     Config
	mux        *http.ServeMux
	httpServer *http.Server
	mu         sync.RWMutex

	// permissions controls which capabilities are exposed. When nil, all
	// routes are allowed (backwards-compatible with existing callers).
	permissions *config.Permissions

	// metering optionally wraps the handler chain with ACET token metering.
	// When non-nil, OpenAI-compatible API requests (/v1/chat/completions,
	// /v1/completions, /v1/embeddings) are metered and billed. Set via
	// SetMetering before Start.
	metering *MeteringMiddleware

	// extraListeners are additional net.Listeners the server will also serve on
	// (e.g., a TLS-wrapped tsnet VPN listener). Added via AddListener before Start.
	extraListeners []net.Listener

	// provisionedResolver maps a /modules/<prefix>/ request to the capability the
	// owning module declared (via the provisioned-service registry). When nil, a
	// module route falls back to the provision capability. Set via
	// SetProvisionedRegistry.
	provisionedResolver CapabilityResolver

	// chatLister, when non-nil, enables the model->engine chat-routing handlers
	// (/v1/chat/completions, /v1/completions, /v1/models). It reports the local
	// serving engines and their models so an incoming chat request is routed to
	// whichever engine serves the requested model. Set via SetChatRouter; see
	// chat_route.go (issue #581, node-side complement of aceteam #6236).
	chatLister ChatModelLister

	// started is set once Start has registered the proxy handlers for the routes
	// present at that moment. It gates WireModuleRoute: a route added AFTER Start
	// must have its proxy handler registered live (Start's registration loop has
	// already run), whereas a route added before Start is picked up by that loop.
	started bool
}

// NewServer creates a new gateway server.
func NewServer(cfg Config) *Server {
	if cfg.Port == 0 {
		cfg.Port = 8443
	}
	if cfg.Upstreams == nil {
		cfg.Upstreams = make(map[string]*Upstream)
	}

	s := &Server{
		config: cfg,
		mux:    http.NewServeMux(),
	}

	return s
}

// AddListener registers an additional net.Listener that the server will also
// serve on when Start is called. For HTTPS gateways, the listener should
// already be TLS-wrapped (e.g., via tls.NewListener). Must be called before Start.
func (s *Server) AddListener(ln net.Listener) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extraListeners = append(s.extraListeners, ln)
}

// AddUpstream registers a backend for the given path prefix.
// Must be called before Start.
func (s *Server) AddUpstream(pathPrefix string, upstream *Upstream) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config.Upstreams[pathPrefix] = upstream
}

// SetUpstreamAddress updates the backend address of an already-registered
// upstream. It is the mechanism a dynamically-provisioned module uses to point
// a route (registered up front with an empty Address) at its real host port once
// that port is known -- a module binds an auto-selected free host port after the
// gateway has started. Returns an error if no upstream is
// registered for the given prefix. Safe to call after Start (reads go through
// the per-request resolveTarget in registerProxy).
func (s *Server) SetUpstreamAddress(prefix, address string) error {
	s.mu.RLock()
	upstream, ok := s.config.Upstreams[prefix]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("gateway: no upstream registered for prefix %q", prefix)
	}
	upstream.setAddr(address)
	return nil
}

// WireModuleRoute points the /modules/<prefix> route at address for a LIVE
// (post-Start) module exposure. It is the re-wire-safe counterpart to
// AddUpstream+SetUpstreamAddress and exists to close the #449 landmine:
//
//   - If the route already exists, it MUTATES the existing *Upstream's address.
//     It must not replace the map entry: registerProxy (run once at Start) closed
//     over the original *Upstream pointer, so swapping in a new object would
//     orphan the update -- the running proxy would keep reading the old object's
//     address. That is exactly why a re-provision that moved the bridge port left
//     the route dialing the dead port until a full restart re-captured it.
//   - If the route is new and the server has already started, it creates the
//     upstream AND registers its proxy handler live (registerProxy), so a module
//     first exposed after Start is reachable without a restart. Before Start, it
//     just records the route; Start's registration loop wires the handler.
//
// stripPrefix applies only when the route is created; for an existing route the
// registration-time options are kept. Safe for concurrent use.
func (s *Server) WireModuleRoute(prefix, address string, stripPrefix bool) error {
	if prefix == "" {
		return fmt.Errorf("gateway: WireModuleRoute requires a non-empty prefix")
	}
	s.mu.Lock()
	upstream, ok := s.config.Upstreams[prefix]
	if !ok {
		upstream = &Upstream{StripPrefix: stripPrefix}
		// Set the address BEFORE the handler can serve traffic so a brand-new route
		// never has a window where its proxy is live but points nowhere (a spurious
		// 502). registerProxy touches only the concurrency-safe mux and closes over
		// this upstream pointer, so the per-request resolveTarget reads this address.
		upstream.setAddr(address)
		s.config.Upstreams[prefix] = upstream
		// A route that appears after Start must have its handler registered here;
		// before Start, the Start loop registers it.
		if s.started {
			s.registerProxy(prefix, upstream)
		}
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	// Existing route: mutate the object the running proxy handler already captured.
	upstream.setAddr(address)
	return nil
}

// SetPermissions sets the capability permissions for route filtering.
// When nil, all routes are allowed. Must be called before Start.
func (s *Server) SetPermissions(p *config.Permissions) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.permissions = p
}

// SetProvisionedRegistry wires the resolver the permission layer uses to gate
// /modules/<prefix>/ requests by the capability the owning module declared.
// Passing nil clears it (module routes then fall back to the provision
// capability). Safe to call after Start (the permission check reads it under the
// lock), so the running gateway can adopt a registry that gains entries over
// time. Typically the caller passes a *provisionedservice.Registry.
func (s *Server) SetProvisionedRegistry(r CapabilityResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.provisionedResolver = r
}

// SetMetering enables ACET token metering on the gateway. When set,
// OpenAI-compatible API requests are intercepted to extract token usage
// and record billing transactions. Must be called before Start.
func (s *Server) SetMetering(m *MeteringMiddleware) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metering = m
}

// modulePrefixFromPath extracts the module prefix from a /modules/<prefix>/...
// (or exact /modules/<prefix>) request path, or "" if the path is not a module
// route. It is the inverse of ModuleRoutePath.
func modulePrefixFromPath(path string) string {
	if !strings.HasPrefix(path, ModuleRoutePrefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, ModuleRoutePrefix)
	if rest == "" {
		return ""
	}
	// The prefix is the first path segment; anything after the next slash is the
	// module's own path.
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return rest
}

// categoryForRequest returns the permission category gating a request path. For
// builtin routes it uses the stable hardcoded mapping (categoryForPath). For a
// /modules/<prefix>/ route it consults the provisioned-service registry (via
// resolver) for the capability the owning module DECLARED, defaulting to
// "provision" when the resolver is nil or the prefix is unknown -- so a module
// route is never left ungated. This is the registry-driven replacement for the
// old hardcoded per-module switch.
func categoryForRequest(path string, resolver CapabilityResolver) string {
	if prefix := modulePrefixFromPath(path); prefix != "" {
		if resolver != nil {
			if capability, found := resolver.CapabilityForPrefix(prefix); found && capability != "" {
				return capability
			}
		}
		// Unknown/registry-less module route: fall back to provision rather than
		// leaving it open (fail closed).
		return "provision"
	}
	return categoryForPath(path)
}

// categoryForPath returns the permission category for a BUILTIN request path, or
// "" if the path should always be allowed (health, status, ping, root). Module
// routes (/modules/<prefix>/) are handled by categoryForRequest, which layers a
// registry lookup on top of this.
func categoryForPath(path string) string {
	// Terminal/console
	if path == "/terminal" || strings.HasPrefix(path, "/terminal/") {
		return "console"
	}
	// Desktop: VNC, screenshots, actions
	if path == "/vnc" || strings.HasPrefix(path, "/vnc/") {
		return "desktop"
	}
	if path == "/api/screenshot" || strings.HasPrefix(path, "/api/screenshot/") {
		return "desktop"
	}
	if path == "/api/actions" || strings.HasPrefix(path, "/api/actions/") {
		return "desktop"
	}
	// Services
	if path == "/services" || strings.HasPrefix(path, "/services/") {
		return "services"
	}
	// SSH
	if path == "/ssh" || strings.HasPrefix(path, "/ssh/") {
		return "ssh"
	}
	// Provisioning
	if path == "/provision" || strings.HasPrefix(path, "/provision/") {
		return "provision"
	}
	// Everything else (health, status, ping, root, unknown) is always allowed.
	// Provisioned-module routes (/modules/<prefix>/) are gated by
	// categoryForRequest via the registry, not here.
	return ""
}

// permissionMiddleware checks permissions before passing requests to the next handler.
// If permissions are nil, all requests pass through.
func (s *Server) permissionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		perms := s.permissions
		resolver := s.provisionedResolver
		s.mu.RUnlock()

		if perms != nil {
			category := categoryForRequest(r.URL.Path, resolver)
			blocked := false
			switch category {
			case "console":
				blocked = !perms.Console
			case "desktop":
				blocked = !perms.Desktop
			case "files":
				blocked = !perms.Files
			case "services":
				blocked = !perms.Services
			case "ssh":
				blocked = !perms.SSH
			case "provision":
				blocked = !perms.Provision
			case "shell":
				blocked = !perms.Shell
			}
			if blocked {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"error":"capability disabled by node operator"}`)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Start begins listening for HTTPS connections. Blocks until context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()

	// Build the route table
	for prefix, upstream := range s.config.Upstreams {
		s.registerProxy(prefix, upstream)
	}
	// Model->engine chat routing (#581): unlike the static upstreams above, the
	// chat routes resolve their backend per request from the requested model, so
	// they are registered by a dedicated method rather than the upstream loop.
	// Same mux, so they are reachable on both the LAN and the VPN listener.
	if s.chatLister != nil {
		s.registerChatRoutes()
	}
	// Any route added after this point (WireModuleRoute) must register its own
	// proxy handler live, since this loop has already run.
	s.started = true

	// Root handler — returns 404 for unmatched paths or gateway info for "/"
	s.mux.HandleFunc("/", s.handleRoot)

	listenAddr := s.config.ListenAddress
	if listenAddr == "" {
		listenAddr = fmt.Sprintf("0.0.0.0:%d", s.config.Port)
	}

	s.httpServer = &http.Server{
		Addr:         listenAddr,
		Handler:      s.BuildHandler(),
		TLSConfig:    s.config.TLSConfig,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second, // Long for WebSocket/streaming
		IdleTimeout:  120 * time.Second,
	}
	s.mu.Unlock()

	// Start listening
	errCh := make(chan error, 1)
	go func() {
		var err error
		if s.config.TLSConfig != nil {
			// TLS mode — certs are in the TLSConfig, so pass empty cert/key paths
			err = s.httpServer.ListenAndServeTLS("", "")
		} else {
			// Plain HTTP fallback (for testing or when --no-tls is set)
			err = s.httpServer.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	// Serve on any extra listeners (e.g., TLS-wrapped tsnet VPN listener)
	for _, ln := range s.extraListeners {
		ln := ln // capture loop variable
		log.Printf("[Gateway] also listening on %s (VPN)", ln.Addr().String())
		go func() {
			if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
				log.Printf("[Gateway] VPN listener error: %v", err)
			}
		}()
	}

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// registerProxy sets up the reverse proxy handler for a given path prefix.
//
// The upstream target is resolved per request (via upstream.addr()) rather than
// captured once, so a dynamically-provisioned upstream whose address is set
// after Start (see SetUpstreamAddress) is honored on the next request without
// re-registering the route. An unset target yields a 502 rather than a panic.
func (s *Server) registerProxy(prefix string, upstream *Upstream) {
	// resolveTarget returns the current upstream URL, or nil when the upstream
	// has no address yet (dynamic upstream not provisioned).
	resolveTarget := func() *url.URL {
		addr := upstream.addr()
		if addr == "" {
			return nil
		}
		target, err := url.Parse("http://" + addr)
		if err != nil {
			log.Printf("[Gateway] invalid upstream address %q for %s: %v", addr, prefix, err)
			return nil
		}
		return target
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			target := resolveTarget()
			if target == nil {
				// Signal unavailability to the transport so ErrorHandler runs.
				req.URL.Scheme = "http"
				req.URL.Host = "gateway-upstream-unset.invalid"
			} else {
				req.URL.Scheme = target.Scheme
				req.URL.Host = target.Host
			}
			req.Header.Set("X-Forwarded-For", req.RemoteAddr)
			req.Header.Set("X-Forwarded-Proto", "https")
			if s.config.NodeName != "" {
				req.Header.Set("X-Citadel-Node", s.config.NodeName)
			}

			if upstream.StripPrefix && prefix != "/" {
				req.URL.Path = strings.TrimPrefix(req.URL.Path, prefix)
				if req.URL.Path == "" {
					req.URL.Path = "/"
				}
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[Gateway] proxy error for %s -> %s: %v", r.URL.Path, upstream.addr(), err)
			http.Error(w, fmt.Sprintf(`{"error":"upstream unavailable","upstream":"%s"}`, prefix), http.StatusBadGateway)
		},
	}

	// For WebSocket upstreams, we need to handle the Upgrade header
	if upstream.WebSocket {
		wsProxy := func(w http.ResponseWriter, r *http.Request) {
			target := resolveTarget()
			if target == nil {
				http.Error(w, fmt.Sprintf(`{"error":"upstream unavailable","upstream":"%s"}`, prefix), http.StatusBadGateway)
				return
			}
			s.proxyWebSocket(w, r, target, prefix, upstream.StripPrefix)
		}
		s.mux.HandleFunc(prefix+"/", func(w http.ResponseWriter, r *http.Request) {
			if isWebSocketUpgrade(r) {
				wsProxy(w, r)
				return
			}
			proxy.ServeHTTP(w, r)
		})
		// Also handle the exact prefix (no trailing slash)
		s.mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
			if isWebSocketUpgrade(r) {
				wsProxy(w, r)
				return
			}
			proxy.ServeHTTP(w, r)
		})
	} else {
		// Register with trailing slash for subtree matching
		s.mux.HandleFunc(prefix+"/", func(w http.ResponseWriter, r *http.Request) {
			proxy.ServeHTTP(w, r)
		})
		// Exact match
		s.mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
			proxy.ServeHTTP(w, r)
		})
	}
}

// handleRoot returns gateway info for "/" or 404 for unmatched paths.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	s.mu.RLock()
	routes := make([]string, 0, len(s.config.Upstreams))
	for prefix := range s.config.Upstreams {
		routes = append(routes, prefix)
	}
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"gateway":"citadel","node":"%s","routes":%d}`, s.config.NodeName, len(routes))
}

// proxyWebSocket handles WebSocket upgrade requests by dialing the upstream
// and doing bidirectional byte copy.
func (s *Server) proxyWebSocket(w http.ResponseWriter, r *http.Request, target *url.URL, prefix string, stripPrefix bool) {
	// Hijack the connection
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket hijack not supported", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf("hijack error: %v", err), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// Dial the upstream
	upstreamConn, err := net.DialTimeout("tcp", target.Host, 10*time.Second)
	if err != nil {
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer upstreamConn.Close()

	// Reconstruct the request to send to upstream
	path := r.URL.Path
	if stripPrefix && prefix != "/" {
		path = strings.TrimPrefix(path, prefix)
		if path == "" {
			path = "/"
		}
	}
	requestURI := path
	if r.URL.RawQuery != "" {
		requestURI = path + "?" + r.URL.RawQuery
	}
	reqLine := fmt.Sprintf("%s %s HTTP/1.1\r\n", r.Method, requestURI)
	clientConn.SetDeadline(time.Time{})

	// Forward the original request headers
	upstreamConn.Write([]byte(reqLine))
	r.Header.Set("Host", target.Host)
	r.Header.Write(upstreamConn)
	upstreamConn.Write([]byte("\r\n"))

	// Bidirectional copy
	done := make(chan struct{}, 2)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := upstreamConn.Read(buf)
			if n > 0 {
				clientConn.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}()
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := clientConn.Read(buf)
			if n > 0 {
				upstreamConn.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}()
	<-done
}

// isWebSocketUpgrade checks if the request is a WebSocket upgrade.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Connection"), "upgrade") &&
		strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// BuildHandler constructs the full middleware chain (logging + permissions +
// optional metering + mux) without starting a server. Useful for testing the
// middleware stack.
func (s *Server) BuildHandler() http.Handler {
	var handler http.Handler = s.mux
	if s.metering != nil {
		handler = s.metering.WrapHandler(handler)
	}
	return s.loggingMiddleware(s.permissionMiddleware(handler))
}

// loggingMiddleware logs all requests.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("[Gateway] %s %s from %s (%s)",
			r.Method, r.URL.Path, r.RemoteAddr, time.Since(start).Round(time.Millisecond))
	})
}
