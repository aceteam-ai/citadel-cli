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
)

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
	Address string

	// StripPrefix removes the matched prefix before forwarding.
	// For example, if the gateway matches "/vnc" and StripPrefix is true,
	// a request to "/vnc/foo" is forwarded as "/foo".
	StripPrefix bool

	// WebSocket indicates this upstream handles WebSocket connections.
	WebSocket bool
}

// Server is the HTTPS reverse proxy gateway.
type Server struct {
	config     Config
	mux        *http.ServeMux
	httpServer *http.Server
	mu         sync.RWMutex
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

// AddUpstream registers a backend for the given path prefix.
// Must be called before Start.
func (s *Server) AddUpstream(pathPrefix string, upstream *Upstream) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config.Upstreams[pathPrefix] = upstream
}

// Start begins listening for HTTPS connections. Blocks until context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()

	// Build the route table
	for prefix, upstream := range s.config.Upstreams {
		s.registerProxy(prefix, upstream)
	}

	// Root handler — returns 404 for unmatched paths or gateway info for "/"
	s.mux.HandleFunc("/", s.handleRoot)

	listenAddr := s.config.ListenAddress
	if listenAddr == "" {
		listenAddr = fmt.Sprintf("0.0.0.0:%d", s.config.Port)
	}

	s.httpServer = &http.Server{
		Addr:         listenAddr,
		Handler:      s.loggingMiddleware(s.mux),
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
func (s *Server) registerProxy(prefix string, upstream *Upstream) {
	target, err := url.Parse("http://" + upstream.Address)
	if err != nil {
		log.Printf("[Gateway] invalid upstream address %q for %s: %v", upstream.Address, prefix, err)
		return
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
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
			log.Printf("[Gateway] proxy error for %s -> %s: %v", r.URL.Path, upstream.Address, err)
			http.Error(w, fmt.Sprintf(`{"error":"upstream unavailable","upstream":"%s"}`, prefix), http.StatusBadGateway)
		},
	}

	// For WebSocket upstreams, we need to handle the Upgrade header
	if upstream.WebSocket {
		s.mux.HandleFunc(prefix+"/", func(w http.ResponseWriter, r *http.Request) {
			if isWebSocketUpgrade(r) {
				s.proxyWebSocket(w, r, target, prefix, upstream.StripPrefix)
				return
			}
			proxy.ServeHTTP(w, r)
		})
		// Also handle the exact prefix (no trailing slash)
		s.mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
			if isWebSocketUpgrade(r) {
				s.proxyWebSocket(w, r, target, prefix, upstream.StripPrefix)
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

// loggingMiddleware logs all requests.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("[Gateway] %s %s from %s (%s)",
			r.Method, r.URL.Path, r.RemoteAddr, time.Since(start).Round(time.Millisecond))
	})
}
