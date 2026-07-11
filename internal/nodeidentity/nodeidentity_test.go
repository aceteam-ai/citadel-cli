package nodeidentity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return New(filepath.Join(t.TempDir(), "identity"))
}

func TestGetOrCreateKey_GeneratesP256WithSecurePerms(t *testing.T) {
	s := newTestStore(t)

	key, err := s.GetOrCreateKey()
	if err != nil {
		t.Fatalf("GetOrCreateKey: %v", err)
	}
	if key.Curve != elliptic.P256() {
		t.Fatalf("expected P-256 curve, got %v", key.Curve.Params().Name)
	}

	info, err := os.Stat(s.KeyPath())
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	// Permission bits are meaningless on Windows; assert only on unix.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("expected key perms 0600, got %o", perm)
		}
	}
}

func TestGetOrCreateKey_Idempotent(t *testing.T) {
	s := newTestStore(t)

	k1, err := s.GetOrCreateKey()
	if err != nil {
		t.Fatalf("first GetOrCreateKey: %v", err)
	}
	k2, err := s.GetOrCreateKey()
	if err != nil {
		t.Fatalf("second GetOrCreateKey: %v", err)
	}
	if k1.D.Cmp(k2.D) != 0 {
		t.Fatal("key changed across calls; expected the same persisted key")
	}
}

func TestKeyRoundTripsFromDisk(t *testing.T) {
	s := newTestStore(t)
	orig, err := s.GetOrCreateKey()
	if err != nil {
		t.Fatalf("GetOrCreateKey: %v", err)
	}

	// Fresh Store pointed at the same dir must load the identical key.
	loaded, err := New(s.Dir()).GetOrCreateKey()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if orig.D.Cmp(loaded.D) != 0 {
		t.Fatal("reloaded private key differs from original")
	}
	if orig.PublicKey.X.Cmp(loaded.PublicKey.X) != 0 || orig.PublicKey.Y.Cmp(loaded.PublicKey.Y) != 0 {
		t.Fatal("reloaded public key differs from original")
	}
}

func TestKeyFileIsPKCS8Private_NotACSR(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.GetOrCreateKey(); err != nil {
		t.Fatalf("GetOrCreateKey: %v", err)
	}
	data, err := os.ReadFile(s.KeyPath())
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("key is not PEM")
	}
	if block.Type != pemTypeECPrivateKey {
		t.Fatalf("expected PEM type %q, got %q", pemTypeECPrivateKey, block.Type)
	}
	if _, err := x509.ParsePKCS8PrivateKey(block.Bytes); err != nil {
		t.Fatalf("key file is not a parseable PKCS#8 private key: %v", err)
	}
}

func TestGenerateCSR_ValidPKCS10WithMatchingPublicKey(t *testing.T) {
	s := newTestStore(t)
	key, err := s.GetOrCreateKey()
	if err != nil {
		t.Fatalf("GetOrCreateKey: %v", err)
	}

	csrPEM, err := s.GenerateCSR(key)
	if err != nil {
		t.Fatalf("GenerateCSR: %v", err)
	}

	block, _ := pem.Decode(csrPEM)
	if block == nil {
		t.Fatal("CSR is not PEM")
	}
	if block.Type != pemTypeCSR {
		t.Fatalf("expected PEM type %q, got %q", pemTypeCSR, block.Type)
	}

	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("CSR self-signature invalid: %v", err)
	}

	csrPub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("CSR public key is not ECDSA: %T", csr.PublicKey)
	}
	if csrPub.X.Cmp(key.PublicKey.X) != 0 || csrPub.Y.Cmp(key.PublicKey.Y) != 0 {
		t.Fatal("CSR public key does not match the node private key")
	}
}

func TestGenerateCSR_NeverLeaksPrivateKey(t *testing.T) {
	s := newTestStore(t)
	key, err := s.GetOrCreateKey()
	if err != nil {
		t.Fatalf("GetOrCreateKey: %v", err)
	}
	csrPEM, err := s.GenerateCSR(key)
	if err != nil {
		t.Fatalf("GenerateCSR: %v", err)
	}

	// The raw private scalar D must not appear anywhere in the CSR bytes.
	dBytes := key.D.Bytes()
	if bytesContains(csrPEM, dBytes) {
		t.Fatal("CSR output contains the private key scalar D")
	}
	// A CSR must contain no PRIVATE KEY PEM block.
	rest := csrPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == pemTypeECPrivateKey || block.Type == "EC PRIVATE KEY" {
			t.Fatalf("CSR PEM unexpectedly contains a private key block %q", block.Type)
		}
	}
}

func TestGenerateCSR_NilKey(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.GenerateCSR(nil); err == nil {
		t.Fatal("expected error for nil key")
	}
}

// makeCert produces a minimal self-signed cert PEM for store/parse tests.
func makeCert(t *testing.T, cn string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: pemTypeCertificate, Bytes: der}))
}

func TestStoreLeaf_PersistsAndParses(t *testing.T) {
	s := newTestStore(t)
	leaf := makeCert(t, "leaf")
	chain := makeCert(t, "intermediate") + makeCert(t, "root")

	if err := s.StoreLeaf(leaf, chain); err != nil {
		t.Fatalf("StoreLeaf: %v", err)
	}
	if !s.HasLeaf() {
		t.Fatal("HasLeaf false after StoreLeaf")
	}

	cert, err := s.LoadLeaf()
	if err != nil {
		t.Fatalf("LoadLeaf: %v", err)
	}
	if cert.Subject.CommonName != "leaf" {
		t.Fatalf("unexpected leaf CN %q", cert.Subject.CommonName)
	}

	// Chain file must contain both certs.
	chainData, err := os.ReadFile(s.CAChainPath())
	if err != nil {
		t.Fatalf("read chain: %v", err)
	}
	if err := validateCertPEM(string(chainData)); err != nil {
		t.Fatalf("stored chain invalid: %v", err)
	}

	if runtime.GOOS != "windows" {
		info, _ := os.Stat(s.LeafPath())
		if perm := info.Mode().Perm(); perm != 0o644 {
			t.Fatalf("expected leaf perms 0644, got %o", perm)
		}
	}
}

func TestStoreLeaf_EmptyIsNoOp(t *testing.T) {
	s := newTestStore(t)
	if err := s.StoreLeaf("", ""); err != nil {
		t.Fatalf("StoreLeaf empty: %v", err)
	}
	if s.HasLeaf() {
		t.Fatal("leaf file created from empty input")
	}
}

func TestStoreLeaf_RejectsMalformedPEM(t *testing.T) {
	s := newTestStore(t)
	if err := s.StoreLeaf("not a cert", ""); err == nil {
		t.Fatal("expected error for malformed leaf PEM")
	}
	if s.HasLeaf() {
		t.Fatal("malformed leaf was written to disk")
	}
}

func bytesContains(haystack, needle []byte) bool {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}
