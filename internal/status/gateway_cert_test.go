package status

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const testCertPEM = `-----BEGIN CERTIFICATE-----
MIIB+zCCAWSgAwIBAgIQ test-gateway-leaf-cert-placeholder
-----END CERTIFICATE-----
`

// TestHandleGatewayCert_ServesPEM verifies that GET /gateway-cert.pem returns the
// on-disk gateway leaf cert PEM with the x-pem-file content type and HTTP 200.
func TestHandleGatewayCert_ServesPEM(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(certPath, []byte(testCertPEM), 0600); err != nil {
		t.Fatalf("write test cert: %v", err)
	}

	server := NewServer(ServerConfig{GatewayCertPath: certPath}, NewCollector(CollectorConfig{}))
	mux := server.buildMux()

	req := httptest.NewRequest(http.MethodGet, "/gateway-cert.pem", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/x-pem-file" {
		t.Errorf("Content-Type = %q, want application/x-pem-file", ct)
	}
	if w.Body.String() != testCertPEM {
		t.Errorf("body = %q, want the cert PEM", w.Body.String())
	}
}

// TestHandleGatewayCert_NoTLS verifies that when no cert path is configured (the
// gateway runs --gateway-no-tls), the endpoint returns 204 No Content so the
// backend falls back to plain http.
func TestHandleGatewayCert_NoTLS(t *testing.T) {
	server := NewServer(ServerConfig{GatewayCertPath: ""}, NewCollector(CollectorConfig{}))
	mux := server.buildMux()

	req := httptest.NewRequest(http.MethodGet, "/gateway-cert.pem", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("body should be empty, got %q", w.Body.String())
	}
}

// TestHandleGatewayCert_MissingFile verifies that a configured-but-missing cert
// file (TLS on, cert not yet written -- a cold-start race) returns 503, NOT 204,
// so the backend retries from cert_refresh_url rather than mis-downgrading to
// plain http against a TLS gateway.
func TestHandleGatewayCert_MissingFile(t *testing.T) {
	server := NewServer(ServerConfig{GatewayCertPath: filepath.Join(t.TempDir(), "absent.pem")}, NewCollector(CollectorConfig{}))
	mux := server.buildMux()

	req := httptest.NewRequest(http.MethodGet, "/gateway-cert.pem", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

// TestHandleGatewayCert_MethodNotAllowed verifies non-GET is rejected.
func TestHandleGatewayCert_MethodNotAllowed(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(certPath, []byte(testCertPEM), 0600); err != nil {
		t.Fatalf("write test cert: %v", err)
	}
	server := NewServer(ServerConfig{GatewayCertPath: certPath}, NewCollector(CollectorConfig{}))
	mux := server.buildMux()

	req := httptest.NewRequest(http.MethodPost, "/gateway-cert.pem", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}
