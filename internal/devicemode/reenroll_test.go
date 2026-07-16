package devicemode

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

	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
)

// testPKI builds an in-memory CA, a server cert for 127.0.0.1, and a
// CA-signed client leaf — the same trust shape as nginx + the fabric CA.
type testPKI struct {
	caPool     *x509.CertPool
	serverCert tls.Certificate
	clientCert tls.Certificate
	clientPEM  []byte
	clientKey  []byte
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-fabric-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	newLeaf := func(cn string, isServer bool) (tls.Certificate, []byte, []byte) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(12 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
		}
		if isServer {
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		} else {
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		return cert, certPEM, keyPEM
	}

	serverCert, _, _ := newLeaf("127.0.0.1", true)
	clientCert, clientPEM, clientKey := newLeaf("device-node-uid", false)

	return &testPKI{
		caPool:     pool,
		serverCert: serverCert,
		clientCert: clientCert,
		clientPEM:  clientPEM,
		clientKey:  clientKey,
	}
}

// startMTLSServer runs an httptest TLS server that REQUIRES a client cert
// chaining to the test CA — mirroring the nginx terminator's contract.
func startMTLSServer(t *testing.T, pki *testPKI, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{pki.serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pki.caPool,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestReenrollSuccess(t *testing.T) {
	pki := newTestPKI(t)
	var sawClientCert bool
	srv := startMTLSServer(t, pki, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			sawClientCert = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok": true, "authkey": "hskey-auth-fresh", "nexus_url": "https://nexus.test", "node_uid": "device-node-uid"}`))
	})

	result, err := reenrollWithRoots(context.Background(), srv.URL, pki.clientCert, pki.caPool)
	if err != nil {
		t.Fatal(err)
	}
	if !sawClientCert {
		t.Fatal("server did not receive the client certificate")
	}
	if result.Authkey != "hskey-auth-fresh" || result.NodeUID != "device-node-uid" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestReenrollWithoutClientCertRejectedAtHandshake(t *testing.T) {
	// Proof-of-possession is the security property: without the private key
	// the TLS handshake itself must fail — the handler is never reached.
	pki := newTestPKI(t)
	srv := startMTLSServer(t, pki, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not be reached without a client cert")
	})

	_, err := reenrollWithRoots(context.Background(), srv.URL, tls.Certificate{}, pki.caPool)
	if err == nil {
		t.Fatal("expected handshake failure without client cert")
	}
}

func TestReenrollErrorMapping(t *testing.T) {
	pki := newTestPKI(t)
	cases := []struct {
		status  int
		body    string
		wantSub string
	}{
		{http.StatusForbidden, `{"error":"certificate revoked"}`, "citadel device enroll"},
		{http.StatusUnauthorized, `{"error":"not trusted"}`, "not trusted"},
		{http.StatusServiceUnavailable, `{"error":"CRL unavailable"}`, "will retry"},
		{http.StatusBadGateway, `{"error":"headscale down"}`, "HTTP 502"},
	}
	for _, tc := range cases {
		srv := startMTLSServer(t, pki, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		_, err := reenrollWithRoots(context.Background(), srv.URL, pki.clientCert, pki.caPool)
		if err == nil {
			t.Fatalf("status %d: expected error", tc.status)
		}
		if !strings.Contains(err.Error(), tc.wantSub) {
			t.Fatalf("status %d: error %q missing %q", tc.status, err.Error(), tc.wantSub)
		}
		srv.Close()
	}
}

func TestReenrollRejectsEmptyAuthkey(t *testing.T) {
	pki := newTestPKI(t)
	srv := startMTLSServer(t, pki, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok": true, "authkey": ""}`))
	})
	if _, err := reenrollWithRoots(context.Background(), srv.URL, pki.clientCert, pki.caPool); err == nil {
		t.Fatal("expected error for empty authkey")
	}
}

func TestLoadClientCertificate(t *testing.T) {
	pki := newTestPKI(t)
	dir := t.TempDir()
	store := nodeidentity.New(dir)

	// Not enrolled: pointed error.
	if _, err := LoadClientCertificate(store); err == nil ||
		!strings.Contains(err.Error(), "citadel device enroll") {
		t.Fatalf("expected not-enrolled error, got %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "node.key"), pki.clientKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node.crt"), pki.clientPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	cert, err := LoadClientCertificate(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("loaded certificate is empty")
	}
}
