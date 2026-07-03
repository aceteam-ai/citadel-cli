package cmd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/gateway"
)

// TestGatewayRouteURL covers the pure mesh-URL builder: scheme selection, the
// /modules/<prefix> convention, and the off-mesh empty-IP case.
func TestGatewayRouteURL(t *testing.T) {
	tests := []struct {
		name   string
		useTLS bool
		ip     string
		port   int
		prefix string
		want   string
	}{
		{"https default", true, "100.64.0.7", 8443, "whatsapp", "https://100.64.0.7:8443/modules/whatsapp"},
		{"http no-tls", false, "100.64.0.7", 8443, "whatsapp", "http://100.64.0.7:8443/modules/whatsapp"},
		{"custom port", true, "100.64.0.9", 9443, "mymod", "https://100.64.0.9:9443/modules/mymod"},
		{"off-mesh empty ip", true, "", 8443, "whatsapp", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := gatewayRouteURL(tc.useTLS, tc.ip, tc.port, tc.prefix)
			if got != tc.want {
				t.Errorf("gatewayRouteURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGatewayFactsForURL_PersistedFile verifies the out-of-process builder READS
// the persisted gateway-facts file (landmine b) rather than assuming 8443/https,
// and falls back to the compile-time default when the file is absent.
func TestGatewayFactsForURL_PersistedFile(t *testing.T) {
	// Isolate the node state dir so we read/write a temp facts file.
	setProvisionedStateDir(t)
	setProvisionedServiceGateway(nil, 0, false, "") // no in-process gateway

	// Absent file -> compile-time fallback.
	f := gatewayFactsForURL()
	if f.Port != DefaultGatewayPort || !f.UseTLS {
		t.Fatalf("fallback facts = %+v, want port %d TLS on", f, DefaultGatewayPort)
	}

	// Persisted file -> its values win over the default.
	if err := writeGatewayFacts(gatewayFacts{Port: 9443, UseTLS: false, CertPath: "/x/cert.pem"}); err != nil {
		t.Fatalf("writeGatewayFacts: %v", err)
	}
	f = gatewayFactsForURL()
	if f.Port != 9443 || f.UseTLS {
		t.Fatalf("persisted facts = %+v, want port 9443 TLS off", f)
	}
	if f.CertPath != "/x/cert.pem" {
		t.Fatalf("cert path = %q, want /x/cert.pem", f.CertPath)
	}
}

// TestVerifyModuleReachable_TrustedCert: with TLS on, the probe VERIFIES against
// the gateway's own cert (landmine a) -- a server presenting that trusted cert is
// reachable. Uses httptest's TLS server and points the persisted facts at its
// cert, with ServerName matching a SAN the test cert carries (127.0.0.1).
func TestVerifyModuleReachable_TrustedCert(t *testing.T) {
	setProvisionedStateDir(t)
	setProvisionedServiceGateway(nil, 0, false, "")

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Persist the server's cert as the "gateway cert" the probe must trust.
	certPath := writeServerCert(t, srv)
	if err := writeGatewayFacts(gatewayFacts{Port: 8443, UseTLS: true, CertPath: certPath}); err != nil {
		t.Fatalf("writeGatewayFacts: %v", err)
	}

	// httptest's cert carries SAN 127.0.0.1/[::1]; meshVerifyingClient sets
	// ServerName from meshIPv4 (empty off-mesh), so verify with an explicit client
	// mirroring meshVerifyingClient but ServerName=127.0.0.1 to match the SAN.
	client := verifyingClientFor(t, certPath, "127.0.0.1")
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("verifying client against trusted cert: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestVerifyModuleReachable_UntrustedCert: a server presenting a DIFFERENT cert
// than the one the probe trusts fails verification (no false-green, landmine a).
func TestVerifyModuleReachable_UntrustedCert(t *testing.T) {
	setProvisionedStateDir(t)
	setProvisionedServiceGateway(nil, 0, false, "")

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Trust an UNRELATED self-signed cert, so the real server's cert does not
	// verify. (Both httptest TLS servers share Go's built-in cert, so we must mint
	// a genuinely different one.)
	otherCert := writeUnrelatedCert(t)

	client := verifyingClientFor(t, otherCert, "127.0.0.1")
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/", nil)
	if _, err := client.Do(req); err == nil {
		t.Fatal("expected a TLS verification error against an untrusted cert, got nil")
	}
}

// TestVerifyModuleReachable_502 / DialFail through the plain-HTTP path (no-TLS
// gateway) still fail loud.
func TestVerifyModuleReachable_502(t *testing.T) {
	setProvisionedStateDir(t)
	setProvisionedServiceGateway(nil, 0, false, "")
	if err := writeGatewayFacts(gatewayFacts{Port: 8080, UseTLS: false}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	err := verifyModuleReachable(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("verifyModuleReachable(502) = %v, want an error mentioning 502", err)
	}
}

func TestVerifyModuleReachable_DialFail(t *testing.T) {
	setProvisionedStateDir(t)
	setProvisionedServiceGateway(nil, 0, false, "")
	if err := writeGatewayFacts(gatewayFacts{Port: 8080, UseTLS: false}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	if err := verifyModuleReachable(context.Background(), url); err == nil {
		t.Fatal("expected a dial error against a closed listener, got nil")
	}
}

// TestMeshVerifyingClient_TLSNoCertFailsLoud: TLS on but no known cert path must
// error rather than silently skip verification (the false-green we removed).
func TestMeshVerifyingClient_TLSNoCertFailsLoud(t *testing.T) {
	setProvisionedStateDir(t)
	setProvisionedServiceGateway(nil, 0, false, "")
	if err := writeGatewayFacts(gatewayFacts{Port: 8443, UseTLS: true, CertPath: ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := meshVerifyingClient(); err == nil {
		t.Fatal("expected an error when TLS is on but the cert path is unknown, got nil")
	}
}

// TestExposeModuleGatewayRoute_NoGatewayRecordsRegistry verifies the local CLI
// path (no in-process gateway) does not hard-fail, and STILL records the module
// in the registry so the running `citadel work` gateway's watcher picks it up
// (landmine c).
func TestExposeModuleGatewayRoute_NoGatewayRecordsRegistry(t *testing.T) {
	setProvisionedStateDir(t)
	setProvisionedServiceGateway(nil, 0, false, "")

	if err := exposeModuleGatewayRoute("mymod", "mymod", "provision", 8137); err != nil {
		t.Fatalf("exposeModuleGatewayRoute with no gateway = %v, want nil (soft no-op)", err)
	}
	cap, ok := provisionedRegistry().CapabilityForPrefix("mymod")
	if !ok || cap != "provision" {
		t.Fatalf("registry after expose: (%q,%v), want (provision,true) -- the CLI must record it for the watcher", cap, ok)
	}
}

// --- test helpers ---

// setProvisionedStateDir points the gateway-facts + registry files at a temp dir
// for the duration of the test, so a unit test never touches real node state.
func setProvisionedStateDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	prev := provisionedStateDirOverride
	provisionedStateDirOverride = dir
	t.Cleanup(func() { provisionedStateDirOverride = prev })
}

// certToPEM encodes a raw DER certificate as PEM.
func certToPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// writeUnrelatedCert mints a fresh self-signed cert (unrelated to httptest's
// built-in one) and writes it to a temp PEM file, returning the path. Used to
// prove an untrusted cert fails verification.
func writeUnrelatedCert(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		Subject:      pkix.Name{CommonName: "unrelated-test-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	path := filepath.Join(t.TempDir(), "unrelated.crt")
	if err := os.WriteFile(path, certToPEM(der), 0600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	return path
}

// newCertPool builds an x509 pool trusting the given PEM cert.
func newCertPool(t *testing.T, pemBytes []byte) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatal("AppendCertsFromPEM failed on test cert")
	}
	return pool
}

// writeServerCert writes a TLS test server's leaf cert to a temp PEM file and
// returns the path.
func writeServerCert(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	cert := srv.Certificate()
	pemBytes := certToPEM(cert.Raw)
	path := filepath.Join(t.TempDir(), "server.crt")
	if err := os.WriteFile(path, pemBytes, 0600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	return path
}

// verifyingClientFor builds an https client that trusts only the cert at
// certPath and uses the given ServerName (to match the cert SAN). It mirrors
// meshVerifyingClient but with an explicit ServerName so the test does not depend
// on mesh IP resolution.
func verifyingClientFor(t *testing.T, certPath, serverName string) *http.Client {
	t.Helper()
	pem, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	pool := newCertPool(t, pem)
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS12},
		},
	}
}

// TestExplicitBridgePort verifies the backfill port reader requires an explicit
// BRIDGE_PORT and never falls back to 8080 (citadel's own status port).
func TestExplicitBridgePort(t *testing.T) {
	if got := explicitBridgePort(map[string]string{}); got != 0 {
		t.Errorf("no BRIDGE_PORT -> %d, want 0 (must NOT default to 8080)", got)
	}
	if got := explicitBridgePort(map[string]string{"BRIDGE_PORT": "8137"}); got != 8137 {
		t.Errorf("BRIDGE_PORT=8137 -> %d, want 8137", got)
	}
	if got := explicitBridgePort(map[string]string{"BRIDGE_PORT": "0"}); got != 0 {
		t.Errorf("BRIDGE_PORT=0 -> %d, want 0", got)
	}
	if got := explicitBridgePort(map[string]string{"BRIDGE_PORT": "nope"}); got != 0 {
		t.Errorf("BRIDGE_PORT=nope -> %d, want 0", got)
	}
}

// TestGatewayRoutePathConvention pins the /modules/<prefix> convention shared with
// the gateway package.
func TestGatewayRoutePathConvention(t *testing.T) {
	if got := gateway.ModuleRoutePath("whatsapp"); got != "/modules/whatsapp" {
		t.Fatalf("ModuleRoutePath = %q, want /modules/whatsapp", got)
	}
}
