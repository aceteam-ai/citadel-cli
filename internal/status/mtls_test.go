package status

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- synthetic fabric CA test fixtures -------------------------------------

type testCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
}

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	return &testCA{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

// issueLeaf signs a client leaf carrying the given SAN URIs (e.g.
// "aceteam:node:<uid>", "aceteam:org:<org>", or a coordinator identity).
func (ca *testCA) issueLeaf(t *testing.T, cn string, sanURIs ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen leaf key: %v", err)
	}
	var uris []*url.URL
	for _, s := range sanURIs {
		u, perr := url.Parse(s)
		if perr != nil {
			t.Fatalf("parse SAN URI %q: %v", s, perr)
		}
		uris = append(uris, u)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         uris,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func writeBundle(t *testing.T, pemBytes []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fabric-ca-bundle.pem")
	if err := os.WriteFile(path, pemBytes, 0600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	return path
}

const coordinatorSAN = "aceteam:role:coordinator"

// --- NewFabricCAVerifier: fail-closed construction -------------------------

func TestNewFabricCAVerifierFailClosed(t *testing.T) {
	ca := newTestCA(t, "fabric-ca")
	bundle := writeBundle(t, ca.certPEM)

	t.Run("empty bundle path rejected", func(t *testing.T) {
		if _, err := NewFabricCAVerifier("", []string{coordinatorSAN}); err == nil {
			t.Fatal("expected error for empty bundle path")
		}
	})
	t.Run("missing bundle file rejected", func(t *testing.T) {
		if _, err := NewFabricCAVerifier(filepath.Join(t.TempDir(), "nope.pem"), []string{coordinatorSAN}); err == nil {
			t.Fatal("expected error for missing bundle")
		}
	})
	t.Run("bundle with no certs rejected", func(t *testing.T) {
		bad := writeBundle(t, []byte("not a pem cert"))
		if _, err := NewFabricCAVerifier(bad, []string{coordinatorSAN}); err == nil {
			t.Fatal("expected error for certless bundle")
		}
	})
	t.Run("empty coordinator allowlist rejected", func(t *testing.T) {
		// Refusing to gate on "any valid fabric cert" is the whole point of #5028.
		if _, err := NewFabricCAVerifier(bundle, nil); err == nil {
			t.Fatal("expected error when no coordinator identities configured")
		}
		if _, err := NewFabricCAVerifier(bundle, []string{"", "  "}); err == nil {
			t.Fatal("expected error when coordinator identities are all blank")
		}
	})
	t.Run("valid construction succeeds", func(t *testing.T) {
		if _, err := NewFabricCAVerifier(bundle, []string{coordinatorSAN}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// --- requireCoordinator: handler-level fail-closed + identity check --------

func TestRequireCoordinatorFailClosed(t *testing.T) {
	ca := newTestCA(t, "fabric-ca")
	bundle := writeBundle(t, ca.certPEM)
	v, err := NewFabricCAVerifier(bundle, []string{coordinatorSAN})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}

	called := false
	next := func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}
	handler := v.requireCoordinator(next)

	coordLeaf := ca.issueLeaf(t, "coordinator", coordinatorSAN)
	peerLeaf := ca.issueLeaf(t, "peer-node", "aceteam:node:abc", "aceteam:org:evil-org")

	cases := []struct {
		name     string
		tlsState *tls.ConnectionState
		wantCode int
		wantNext bool
	}{
		{"no TLS at all", nil, http.StatusForbidden, false},
		{"TLS but no peer cert", &tls.ConnectionState{}, http.StatusForbidden, false},
		{
			"valid fabric cert but not a coordinator (peer org node)",
			&tls.ConnectionState{PeerCertificates: []*x509.Certificate{peerLeaf.Leaf}},
			http.StatusForbidden, false,
		},
		{
			"coordinator identity accepted",
			&tls.ConnectionState{PeerCertificates: []*x509.Certificate{coordLeaf.Leaf}},
			http.StatusOK, true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			req := httptest.NewRequest(http.MethodPost, "/ssh/authorized-keys", nil)
			req.TLS = tc.tlsState
			w := httptest.NewRecorder()
			handler(w, req)
			if w.Code != tc.wantCode {
				t.Errorf("code = %d, want %d", w.Code, tc.wantCode)
			}
			if called != tc.wantNext {
				t.Errorf("next called = %v, want %v (fail-closed means next must NOT run)", called, tc.wantNext)
			}
		})
	}
}

// --- end-to-end: SSH-key injection over the mTLS control listener ----------

// startControlTLSServer serves the control mux (SSH-key injection gated by
// requireCoordinator) over a real TLS handshake with RequireAndVerifyClientCert,
// exactly as the node's control listener does.
func startControlTLSServer(t *testing.T, v *FabricCAVerifier, serverCert tls.Certificate) *httptest.Server {
	t.Helper()
	collector := NewCollector(CollectorConfig{NodeName: "test-node"})
	srv := NewServer(ServerConfig{CAVerifier: v, ControlServerCert: &serverCert}, collector)
	ts := httptest.NewUnstartedServer(srv.buildControlMux())
	ts.TLS = v.ServerTLSConfig(serverCert)
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func clientWithCert(ts *httptest.Server, clientCert *tls.Certificate) *http.Client {
	tr := ts.Client().Transport.(*http.Transport).Clone()
	// These tests exercise the SERVER verifying the CLIENT cert (mTLS client
	// auth). Verifying the node's server cert is a separate concern, so skip it
	// here -- the synthetic server leaf has no 127.0.0.1 SAN.
	tr.TLSClientConfig.InsecureSkipVerify = true
	if clientCert != nil {
		tr.TLSClientConfig.Certificates = []tls.Certificate{*clientCert}
	}
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}
}

func TestSSHInjectionOverMTLS(t *testing.T) {
	// Isolate HOME so deploySSHKeys writes into a temp dir we can inspect.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	authKeysPath := filepath.Join(tmpHome, ".ssh", "authorized_keys")

	ca := newTestCA(t, "fabric-ca")
	otherCA := newTestCA(t, "attacker-ca")
	bundle := writeBundle(t, ca.certPEM)
	v, err := NewFabricCAVerifier(bundle, []string{coordinatorSAN})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	serverCert := ca.issueLeaf(t, "node-server")

	ts := startControlTLSServer(t, v, serverCert)

	validKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl coordinator@aceteam"
	postKeys := func(client *http.Client) (int, error) {
		body, _ := json.Marshal(sshAuthorizedKeysRequest{Keys: []string{validKey}})
		resp, err := client.Post(ts.URL+"/ssh/authorized-keys", "application/json", bytes.NewReader(body))
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		return resp.StatusCode, nil
	}
	fileHasKey := func() bool {
		data, err := os.ReadFile(authKeysPath)
		return err == nil && strings.Contains(string(data), validKey)
	}

	t.Run("no client cert is rejected at handshake (fail closed)", func(t *testing.T) {
		_, err := postKeys(clientWithCert(ts, nil))
		if err == nil {
			t.Fatal("expected TLS handshake failure without a client cert")
		}
		if fileHasKey() {
			t.Fatal("authorized_keys must not be written without a coordinator cert")
		}
	})

	t.Run("client cert from an untrusted CA is rejected at handshake", func(t *testing.T) {
		attackerCert := otherCA.issueLeaf(t, "attacker", coordinatorSAN)
		_, err := postKeys(clientWithCert(ts, &attackerCert))
		if err == nil {
			t.Fatal("expected handshake failure for a cert not chaining to the fabric CA")
		}
		if fileHasKey() {
			t.Fatal("authorized_keys must not be written for an untrusted-CA cert")
		}
	})

	t.Run("valid fabric cert without coordinator SAN is rejected (403, no write)", func(t *testing.T) {
		peerCert := ca.issueLeaf(t, "peer-node", "aceteam:node:xyz", "aceteam:org:some-other-org")
		code, err := postKeys(clientWithCert(ts, &peerCert))
		if err != nil {
			t.Fatalf("request error: %v", err)
		}
		if code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", code)
		}
		if fileHasKey() {
			t.Fatal("authorized_keys must not be written for a non-coordinator identity")
		}
	})

	t.Run("coordinator cert is accepted and injects the key", func(t *testing.T) {
		coordCert := ca.issueLeaf(t, "coordinator", coordinatorSAN)
		code, err := postKeys(clientWithCert(ts, &coordCert))
		if err != nil {
			t.Fatalf("request error: %v", err)
		}
		if code != http.StatusOK {
			t.Fatalf("code = %d, want 200", code)
		}
		if !fileHasKey() {
			t.Fatal("authorized_keys should contain the injected key after a coordinator call")
		}
	})
}

// --- default coordinator SAN: cross-repo contract pin (#5028) --------------

// TestDefaultCoordinatorSANContract pins the exact SAN literal that the backend
// fabric CA (python-backend profile.COORDINATOR_URI) mints into the relay's
// client cert. A drift on either side silently breaks SSH-deploy fleetwide, so
// this asserts the literal string AND that a leaf carrying it is authorized when
// the verifier is built with the default, while a node identity is not.
func TestDefaultCoordinatorSANContract(t *testing.T) {
	if DefaultCoordinatorSAN != "aceteam:coordinator" {
		t.Fatalf("DefaultCoordinatorSAN = %q, want %q (must match backend COORDINATOR_URI)",
			DefaultCoordinatorSAN, "aceteam:coordinator")
	}

	ca := newTestCA(t, "fabric-ca")
	bundle := writeBundle(t, ca.certPEM)
	v, err := NewFabricCAVerifier(bundle, []string{DefaultCoordinatorSAN})
	if err != nil {
		t.Fatalf("verifier with default SAN: %v", err)
	}

	coordLeaf := ca.issueLeaf(t, "coordinator", DefaultCoordinatorSAN)
	if !v.isCoordinator(coordLeaf.Leaf) {
		t.Error("a leaf carrying the default coordinator SAN must be authorized")
	}

	nodeLeaf := ca.issueLeaf(t, "some-node", "aceteam:node:abc", "aceteam:org:some-org")
	if v.isCoordinator(nodeLeaf.Leaf) {
		t.Error("a node identity must NOT be authorized as the coordinator")
	}
}
