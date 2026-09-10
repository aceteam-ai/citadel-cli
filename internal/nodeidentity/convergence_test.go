package nodeidentity

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These tests pin the K-A key-store convergence + read-through adoption
// (docs/design-trust-receipt-v2.md §4, aceteam#8253). They use the in-package
// newStore(dir, legacyDir) seam so BOTH the convergent dir and the "legacy"
// dir are temp dirs — never the real platform.ConfigDir()/network dirs (which,
// on this dev box, are node 1297's live identity paths).

func selfSignedCertPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen cert key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "test-leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: pemTypeCertificate, Bytes: der}))
}

// seedIdentity writes a fresh key (and optionally a leaf + CA chain) into an
// identity dir WITHOUT any read-through (New has legacyDir==""), standing in
// for a key registered there by a pre-convergence citadel. Returns the exact
// on-disk key bytes so a caller can assert byte-identical adoption.
func seedIdentity(t *testing.T, identityDir string, withLeaf bool) []byte {
	t.Helper()
	s := New(identityDir)
	if _, err := s.GetOrCreateKey(); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	if withLeaf {
		if err := s.StoreLeaf(selfSignedCertPEM(t), selfSignedCertPEM(t)); err != nil {
			t.Fatalf("seed leaf/chain: %v", err)
		}
	}
	return mustReadFile(t, s.KeyPath())
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func writeKeyBytes(t *testing.T, identityDir string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(identityDir, dirPerms); err != nil {
		t.Fatalf("mkdir %s: %v", identityDir, err)
	}
	if err := os.WriteFile(filepath.Join(identityDir, keyFileName), data, keyPerms); err != nil {
		t.Fatalf("write key bytes: %v", err)
	}
}

func displacedSiblings(t *testing.T, identityDir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(identityDir, keyFileName+".displaced-*"))
	if err != nil {
		t.Fatalf("glob displaced: %v", err)
	}
	return matches
}

// TestConvergent_AdoptsLegacyKeyByteIdentical is the core invariant: a fresh
// convergent store adopts a legacy-registered key byte-for-byte, so the SPKI
// (and thus any already-issued CA leaf) is preserved — no re-pair.
func TestConvergent_AdoptsLegacyKeyByteIdentical(t *testing.T) {
	convergent := filepath.Join(t.TempDir(), "identity")
	legacy := filepath.Join(t.TempDir(), "identity")
	legacyBytes := seedIdentity(t, legacy, false)

	s := newStore(convergent, legacy)
	if _, err := s.GetOrCreateKey(); err != nil {
		t.Fatalf("GetOrCreateKey: %v", err)
	}

	got := mustReadFile(t, s.KeyPath())
	if !bytes.Equal(got, legacyBytes) {
		t.Fatalf("adopted key is not byte-identical to the legacy registered key")
	}
	if len(displacedSiblings(t, convergent)) != 0 {
		t.Fatalf("no key was displaced on a fresh convergent dir; unexpected sibling")
	}
}

// TestConvergent_SignerAndCSRResolveSameKeyAfterConvergence pins the whole
// point of K-A: the AEP receipt signer's store and the CSR/enrollment store
// (both Convergent, here modeled as two newStore instances rooted at the SAME
// convergent dir with the SAME legacy dir) resolve the IDENTICAL key — and it
// is the legacy-registered key.
func TestConvergent_SignerAndCSRResolveSameKeyAfterConvergence(t *testing.T) {
	convergent := filepath.Join(t.TempDir(), "identity")
	legacy := filepath.Join(t.TempDir(), "identity")
	legacyBytes := seedIdentity(t, legacy, false)

	signerStore := newStore(convergent, legacy) // stands in for defaultAEPSigner()
	csrStore := newStore(convergent, legacy)    // stands in for the CSR/enroll path

	signerKey, err := signerStore.GetOrCreateKey()
	if err != nil {
		t.Fatalf("signer GetOrCreateKey: %v", err)
	}
	csrKey, err := csrStore.GetOrCreateKey()
	if err != nil {
		t.Fatalf("csr GetOrCreateKey: %v", err)
	}

	if signerKey.D.Cmp(csrKey.D) != 0 {
		t.Fatalf("signer and CSR keys differ — a signed receipt's fingerprint could never match a fabric_node_certs row")
	}
	fpSigner, err := signerStore.PublicKeyFingerprint()
	if err != nil {
		t.Fatalf("signer fingerprint: %v", err)
	}
	fpCSR, err := csrStore.PublicKeyFingerprint()
	if err != nil {
		t.Fatalf("csr fingerprint: %v", err)
	}
	if fpSigner != fpCSR {
		t.Fatalf("fingerprints diverge: %q vs %q", fpSigner, fpCSR)
	}
	if got := mustReadFile(t, signerStore.KeyPath()); !bytes.Equal(got, legacyBytes) {
		t.Fatalf("resolved key is not the legacy-registered key")
	}
}

// TestConvergent_MachineConvergentAcrossInvocationContexts pins that two
// independently-constructed convergent stores rooted at the SAME convergent
// dir (systemd-root `citadel work` vs interactive `citadel init`) load/create
// the identical key even with NO legacy key present (fresh node).
func TestConvergent_MachineConvergentAcrossInvocationContexts(t *testing.T) {
	convergent := filepath.Join(t.TempDir(), "identity")
	legacy := filepath.Join(t.TempDir(), "identity") // empty: no legacy key

	a := newStore(convergent, legacy)
	keyA, err := a.GetOrCreateKey()
	if err != nil {
		t.Fatalf("context A: %v", err)
	}
	b := newStore(convergent, legacy)
	keyB, err := b.GetOrCreateKey()
	if err != nil {
		t.Fatalf("context B: %v", err)
	}
	if keyA.D.Cmp(keyB.D) != 0 {
		t.Fatalf("two invocation contexts on the same convergent dir loaded DIFFERENT keys")
	}
}

// TestConvergent_FreshNodeGeneratesExactlyOneKeyAtConvergentLocation: with no
// legacy key, exactly one key is minted, at the convergent dir, and nothing is
// written to the legacy dir or left as a displaced sibling.
func TestConvergent_FreshNodeGeneratesExactlyOneKeyAtConvergentLocation(t *testing.T) {
	convergent := filepath.Join(t.TempDir(), "identity")
	legacy := filepath.Join(t.TempDir(), "identity")

	s := newStore(convergent, legacy)
	if _, err := s.GetOrCreateKey(); err != nil {
		t.Fatalf("GetOrCreateKey: %v", err)
	}

	if _, err := os.Stat(filepath.Join(convergent, keyFileName)); err != nil {
		t.Fatalf("expected a key at the convergent location: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, keyFileName)); !os.IsNotExist(err) {
		t.Fatalf("a fresh-node key must NOT be written to the legacy dir (err=%v)", err)
	}
	if len(displacedSiblings(t, convergent)) != 0 {
		t.Fatalf("fresh node must not produce a displaced sibling")
	}
}

// TestConvergent_Idempotent: adopting, then re-running, keeps the same key and
// never produces a second/displaced key.
func TestConvergent_Idempotent(t *testing.T) {
	convergent := filepath.Join(t.TempDir(), "identity")
	legacy := filepath.Join(t.TempDir(), "identity")
	seedIdentity(t, legacy, true) // key + leaf + chain

	s := newStore(convergent, legacy)
	k1, err := s.GetOrCreateKey()
	if err != nil {
		t.Fatalf("first GetOrCreateKey: %v", err)
	}
	first := mustReadFile(t, s.KeyPath())

	k2, err := newStore(convergent, legacy).GetOrCreateKey()
	if err != nil {
		t.Fatalf("second GetOrCreateKey: %v", err)
	}
	if k1.D.Cmp(k2.D) != 0 {
		t.Fatalf("key changed across convergent re-runs")
	}
	if second := mustReadFile(t, s.KeyPath()); !bytes.Equal(first, second) {
		t.Fatalf("convergent key bytes changed across re-runs")
	}
	if len(displacedSiblings(t, convergent)) != 0 {
		t.Fatalf("idempotent re-run must not displace a key")
	}
}

// TestConvergent_NeverOverwritesRegisteredConvergentKey is advisor pin (b): a
// convergent key that ALREADY carries a leaf (a post-K-A registration here) is
// FINAL and is never displaced by a differing legacy key.
func TestConvergent_NeverOverwritesRegisteredConvergentKey(t *testing.T) {
	convergent := filepath.Join(t.TempDir(), "identity")
	legacy := filepath.Join(t.TempDir(), "identity")

	// Convergent already registered here: key + leaf.
	registeredBytes := seedIdentity(t, convergent, true)
	// A DIFFERENT key sits at the legacy location.
	seedIdentity(t, legacy, false)

	s := newStore(convergent, legacy)
	key, err := s.GetOrCreateKey()
	if err != nil {
		t.Fatalf("GetOrCreateKey: %v", err)
	}

	if got := mustReadFile(t, s.KeyPath()); !bytes.Equal(got, registeredBytes) {
		t.Fatalf("a registered convergent key (key+leaf) was overwritten by the legacy key")
	}
	// The returned key must be the registered one, not the legacy one.
	legacyKey, err := New(legacy).GetOrCreateKey()
	if err != nil {
		t.Fatalf("load legacy: %v", err)
	}
	if key.D.Cmp(legacyKey.D) == 0 {
		t.Fatalf("resolved key is the legacy key; the registered convergent key should win")
	}
	if len(displacedSiblings(t, convergent)) != 0 {
		t.Fatalf("a registered key must not be displaced")
	}
}

// TestConvergent_DisplacesLeaflessConvergentKeyForRegisteredLegacy is advisor
// pin (a): a LEAFLESS convergent key (a bare #917 AEP signer key, never CA
// registered) is displaced by a differing legacy key — but copied aside first,
// never destroyed.
func TestConvergent_DisplacesLeaflessConvergentKeyForRegisteredLegacy(t *testing.T) {
	convergent := filepath.Join(t.TempDir(), "identity")
	legacy := filepath.Join(t.TempDir(), "identity")

	oldConvergentBytes := seedIdentity(t, convergent, false) // leafless signer key
	legacyBytes := seedIdentity(t, legacy, false)            // different, registered key

	if bytes.Equal(oldConvergentBytes, legacyBytes) {
		t.Fatal("test setup: expected the two seeded keys to differ")
	}

	s := newStore(convergent, legacy)
	if _, err := s.GetOrCreateKey(); err != nil {
		t.Fatalf("GetOrCreateKey: %v", err)
	}

	if got := mustReadFile(t, s.KeyPath()); !bytes.Equal(got, legacyBytes) {
		t.Fatalf("convergent key was not replaced by the registered legacy key")
	}
	siblings := displacedSiblings(t, convergent)
	if len(siblings) != 1 {
		t.Fatalf("expected exactly 1 displaced sibling, got %d", len(siblings))
	}
	if got := mustReadFile(t, siblings[0]); !bytes.Equal(got, oldConvergentBytes) {
		t.Fatalf("displaced sibling does not hold the OLD convergent key bytes")
	}
}

// TestConvergent_NoOpWhenConvergentEqualsLegacy is advisor pin (c): if the
// convergent key already equals the legacy key, nothing is copied or displaced.
func TestConvergent_NoOpWhenConvergentEqualsLegacy(t *testing.T) {
	convergent := filepath.Join(t.TempDir(), "identity")
	legacy := filepath.Join(t.TempDir(), "identity")

	shared := seedIdentity(t, legacy, false)
	writeKeyBytes(t, convergent, shared) // same bytes already at convergent

	s := newStore(convergent, legacy)
	if _, err := s.GetOrCreateKey(); err != nil {
		t.Fatalf("GetOrCreateKey: %v", err)
	}
	if got := mustReadFile(t, s.KeyPath()); !bytes.Equal(got, shared) {
		t.Fatalf("convergent key changed despite already equaling legacy")
	}
	if len(displacedSiblings(t, convergent)) != 0 {
		t.Fatalf("no displacement expected when convergent already equals legacy")
	}
}

// TestConvergent_HardErrorNeverGeneratesSecondKeyWhenLegacyUnreadable pins the
// load-bearing invariant: if a legacy key is present but adoption fails,
// GetOrCreateKey returns an ERROR and never silently mints a fresh convergent
// key beside a (possibly CA-registered) legacy one.
func TestConvergent_HardErrorNeverGeneratesSecondKeyWhenLegacyUnreadable(t *testing.T) {
	convergent := filepath.Join(t.TempDir(), "identity")
	legacy := filepath.Join(t.TempDir(), "identity")

	// Make the legacy node.key unreadable-as-a-file by making it a DIRECTORY:
	// os.ReadFile then fails with a non-IsNotExist error, so reconcile must
	// hard-fail rather than treat it as "no legacy key".
	if err := os.MkdirAll(filepath.Join(legacy, keyFileName), dirPerms); err != nil {
		t.Fatalf("setup: %v", err)
	}

	s := newStore(convergent, legacy)
	if _, err := s.GetOrCreateKey(); err == nil {
		t.Fatalf("expected a hard error when the legacy key is present but unreadable")
	}
	if _, err := os.Stat(filepath.Join(convergent, keyFileName)); !os.IsNotExist(err) {
		t.Fatalf("a fresh convergent key must NOT be generated when legacy adoption failed (err=%v)", err)
	}
}

// TestConvergent_AdoptsLegacyLeafAndChain pins invariant #3 (CSR/device flows
// keep working): an already-paired node's leaf + CA chain migrate alongside the
// key, so LoadLeaf resolves the adopted leaf.
func TestConvergent_AdoptsLegacyLeafAndChain(t *testing.T) {
	convergent := filepath.Join(t.TempDir(), "identity")
	legacy := filepath.Join(t.TempDir(), "identity")
	seedIdentity(t, legacy, true) // key + leaf + chain

	s := newStore(convergent, legacy)
	// LoadLeaf triggers the best-effort read-through even without a key access.
	if _, err := s.LoadLeaf(); err != nil {
		t.Fatalf("LoadLeaf after convergence: %v", err)
	}
	for _, name := range []string{keyFileName, leafFileName, caChainFileName} {
		if _, err := os.Stat(filepath.Join(convergent, name)); err != nil {
			t.Fatalf("expected %s adopted into convergent dir: %v", name, err)
		}
	}
}

// TestNewStore_SelfLegacyDirDisablesReadThrough: a legacyDir equal to dir must
// blank out (no self-adoption / infinite reference).
func TestNewStore_SelfLegacyDirDisablesReadThrough(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	s := newStore(dir, dir)
	if s.legacyDir != "" {
		t.Fatalf("legacyDir == dir must be blanked, got %q", s.legacyDir)
	}
}
