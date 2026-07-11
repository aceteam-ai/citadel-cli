package cmd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func selfSignedPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "test-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// TestEnsureNodeIdentity_WiringSmoke confirms the init-path wiring actually
// generates a 0600 key and caches the CA chain via a mock backend.
func TestEnsureNodeIdentity_WiringSmoke(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	chain := selfSignedPEM(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/fabric/ca/chain" {
			_, _ = w.Write([]byte(chain))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	ensureNodeIdentity(srv.URL)

	keyPath := filepath.Join(home, ".citadel-cli", "identity", "node.key")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("expected key at %s: %v", keyPath, err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("key perms = %o, want 0600", info.Mode().Perm())
	}
	chainPath := filepath.Join(home, ".citadel-cli", "identity", "ca-chain.pem")
	if _, err := os.Stat(chainPath); err != nil {
		t.Fatalf("expected CA chain cached: %v", err)
	}
}

// TestEnsureNodeIdentity_FailOpenOn503 confirms a 503 CA does not error out and
// still leaves a key (fail-open).
func TestEnsureNodeIdentity_FailOpenOn503(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ensureNodeIdentity(srv.URL) // must not panic/exit

	keyPath := filepath.Join(home, ".citadel-cli", "identity", "node.key")
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key should exist even when CA is 503: %v", err)
	}
	chainPath := filepath.Join(home, ".citadel-cli", "identity", "ca-chain.pem")
	if _, err := os.Stat(chainPath); err == nil {
		t.Fatal("chain should NOT be written on 503")
	}
}
