package devicemode

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
)

// renewTestCA mints leaves bound to a CALLER-SUPPLIED public key — renewal's
// whole contract is "same key, new cert", so the test CA must be able to
// issue for the device's existing key (unlike testPKI's self-generated leaves).
type renewTestCA struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
}

func newRenewCA(t *testing.T) *renewTestCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "renew-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(48 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &renewTestCA{key: key, cert: cert}
}

func (ca *renewTestCA) issue(t *testing.T, pub *ecdsa.PublicKey, notAfter time.Time) []byte {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "device-leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// seedStore writes a device key + CA-issued leaf into a fresh identity store,
// returning the store and the device key.
func seedStore(t *testing.T, ca *renewTestCA) (*nodeidentity.Store, *ecdsa.PrivateKey) {
	t.Helper()
	dir := t.TempDir()
	store := nodeidentity.New(dir)
	key, err := store.GetOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	leafPEM := ca.issue(t, &key.PublicKey, time.Now().Add(20*24*time.Hour))
	if err := os.WriteFile(filepath.Join(dir, "node.crt"), leafPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	return store, key
}

func TestRenewLeafSuccess(t *testing.T) {
	ca := newRenewCA(t)
	store, key := seedStore(t, ca)
	oldLeaf, err := store.LoadLeaf()
	if err != nil {
		t.Fatal(err)
	}

	newNotAfter := time.Now().Add(365 * 24 * time.Hour).Truncate(time.Second)
	var gotReq renewRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/fabric/ca/renew" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Errorf("decode request: %v", err)
		}
		// The wire shape carries the leaf + a CSR self-signed by the SAME
		// key — verify the possession material server-side like the platform
		// endpoint does.
		block, _ := pem.Decode([]byte(gotReq.CSRPem))
		if block == nil {
			t.Error("request csr_pem is not PEM")
		} else if csr, err := x509.ParseCertificateRequest(block.Bytes); err != nil {
			t.Errorf("parse CSR: %v", err)
		} else if err := csr.CheckSignature(); err != nil {
			t.Errorf("CSR self-signature invalid: %v", err)
		} else if !csr.PublicKey.(*ecdsa.PublicKey).Equal(&key.PublicKey) {
			t.Error("CSR key does not match the device key")
		}
		resp := renewResponse{
			OK:      true,
			LeafPem: string(ca.issue(t, &key.PublicKey, newNotAfter)),
			NodeUID: "device-node-uid",
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	notAfter, err := RenewLeaf(context.Background(), srv.Client(), srv.URL, store)
	if err != nil {
		t.Fatal(err)
	}
	if !notAfter.After(oldLeaf.NotAfter) {
		t.Fatalf("renewed notAfter %s not after old %s", notAfter, oldLeaf.NotAfter)
	}
	// The private key must NEVER ride the wire.
	if strings.Contains(gotReq.LeafPem+gotReq.CSRPem, "PRIVATE KEY") {
		t.Fatal("request payload contains private key material")
	}
	// The store now holds the successor.
	stored, err := store.LoadLeaf()
	if err != nil {
		t.Fatal(err)
	}
	if !stored.NotAfter.Equal(notAfter) {
		t.Fatalf("stored leaf notAfter %s != returned %s", stored.NotAfter, notAfter)
	}
}

func TestRenewLeafRejectsForeignKeyLeaf(t *testing.T) {
	// A response leaf bound to a DIFFERENT key must be refused and the
	// existing (working) leaf left untouched — storing it would replace a
	// usable identity with one we hold no key for.
	ca := newRenewCA(t)
	store, _ := seedStore(t, ca)
	oldLeaf, _ := store.LoadLeaf()

	foreignKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := renewResponse{
			OK:      true,
			LeafPem: string(ca.issue(t, &foreignKey.PublicKey, time.Now().Add(365*24*time.Hour))),
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	if _, err := RenewLeaf(context.Background(), srv.Client(), srv.URL, store); err == nil {
		t.Fatal("expected error for foreign-key leaf")
	} else if !strings.Contains(err.Error(), "not bound to this device's key") {
		t.Fatalf("unexpected error: %v", err)
	}
	stored, _ := store.LoadLeaf()
	if !stored.NotAfter.Equal(oldLeaf.NotAfter) {
		t.Fatal("store was overwritten with an unusable leaf")
	}
}

func TestRenewLeafErrorMapping(t *testing.T) {
	ca := newRenewCA(t)
	cases := []struct {
		status  int
		wantSub string
	}{
		{http.StatusForbidden, "citadel device enroll"},
		{http.StatusUnauthorized, "not recognized"},
		{http.StatusTooManyRequests, "rate-limited"},
		{http.StatusServiceUnavailable, "will retry"},
		{http.StatusBadGateway, "HTTP 502"},
	}
	for _, tc := range cases {
		store, _ := seedStore(t, ca)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"detail":"nope"}`))
		}))
		_, err := RenewLeaf(context.Background(), srv.Client(), srv.URL, store)
		if err == nil {
			t.Fatalf("status %d: expected error", tc.status)
		}
		if !strings.Contains(err.Error(), tc.wantSub) {
			t.Fatalf("status %d: error %q missing %q", tc.status, err.Error(), tc.wantSub)
		}
		srv.Close()
	}
}

func TestRenewLeafNotEnrolled(t *testing.T) {
	store := nodeidentity.New(t.TempDir())
	_, err := RenewLeaf(context.Background(), http.DefaultClient, "http://unused", store)
	if err == nil || !strings.Contains(err.Error(), "citadel device enroll") {
		t.Fatalf("expected not-enrolled error, got %v", err)
	}
}

func TestRenewLeafGarbageResponse(t *testing.T) {
	ca := newRenewCA(t)
	store, _ := seedStore(t, ca)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok": true, "leaf_pem": "not a pem"}`))
	}))
	defer srv.Close()

	if _, err := RenewLeaf(context.Background(), srv.Client(), srv.URL, store); err == nil {
		t.Fatal("expected error for garbage leaf in response")
	}
}
