package ingress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// Server assembles the public listener's handler: it routes the reserved health
// host to the health check and everything else to the reverse proxy, and serves
// HTTPS with the CertProvider's TLS config.
type Server struct {
	appsDomain  string
	healthHost  string
	proxy       http.Handler
	cert        CertProvider
	isConnected func() bool // mesh connectivity (network.IsGlobalConnected in prod)
	routesReady func() bool // routes fetched at least once (Client.FetchedOnce)
	logf        func(format string, args ...any)
}

// ServerConfig configures a Server.
type ServerConfig struct {
	AppsDomain  string
	Proxy       http.Handler
	Cert        CertProvider
	IsConnected func() bool
	RoutesReady func() bool
	Logf        func(format string, args ...any)
}

// NewServer builds a Server.
func NewServer(cfg ServerConfig) *Server {
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Server{
		appsDomain: cfg.AppsDomain,
		// The health endpoint lives on the PUBLIC listener under a reserved host
		// (per the DoR health-check correction), NOT a loopback /healthz: the
		// redundancy nameserver probes each ingress over the public path and uses
		// the wildcard-cert TLS handshake as origin authenticity. `_health` can
		// never collide with a slug -- the underscore fails slugRegexp.
		healthHost:  "_health." + cfg.AppsDomain,
		proxy:       cfg.Proxy,
		cert:        cfg.Cert,
		isConnected: cfg.IsConnected,
		routesReady: cfg.RoutesReady,
		logf:        logf,
	}
}

// Handler is the top-level HTTP handler for the public listener.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hostWithoutPort(r.Host) == s.healthHost {
			s.handleHealth(w)
			return
		}
		s.proxy.ServeHTTP(w, r)
	})
}

// handleHealth returns 200 only when the mesh is connected AND routes have been
// fetched at least once. The wildcard cert being loaded is proven implicitly:
// startup blocks on CertProvider.Start before the listener serves, so a
// completed TLS handshake IS the "cert loaded" signal. The body carries nothing
// beyond the status word -- no route count, no mesh IP.
func (s *Server) handleHealth(w http.ResponseWriter) {
	ready := s.isConnected != nil && s.isConnected() &&
		s.routesReady != nil && s.routesReady()
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready\n"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// Serve serves HTTPS on ln until ctx is cancelled, then shuts down gracefully.
// The caller supplies the listener (in production a plain host net.Listen on
// :443; in tests a 127.0.0.1:0 listener) so this never binds a public port on
// its own. CertProvider.Start must have already succeeded.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:   s.Handler(),
		TLSConfig: s.cert.TLSConfig(),
		// ReadHeaderTimeout bounds slow-loris on the header; IdleTimeout reaps
		// idle keep-alives. WriteTimeout is deliberately 0: a non-zero
		// WriteTimeout silently kills SSE streams and long-lived WebSockets.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		// Empty cert/key filenames are valid because TLSConfig.GetCertificate is
		// set (by the CertProvider).
		errCh <- srv.ServeTLS(ln, "", "")
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// RedirectToHTTPS is an optional :80 handler that 308-redirects to the https URL
// of the same host+path. It is a pure handler so it is testable without binding
// :80; the cmd layer wires it only when --http-listen is set.
func RedirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	host := hostWithoutPort(r.Host)
	target := "https://" + host + r.URL.RequestURI()
	http.Redirect(w, r, target, http.StatusPermanentRedirect)
}
