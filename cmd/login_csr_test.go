package cmd

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
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
)

const testLoginCSR = "-----BEGIN CERTIFICATE REQUEST-----\nMIIBcsrbytes\n-----END CERTIFICATE REQUEST-----\n"

// TestRunDeviceAuthFlowWithCSR_ThreadsCSRThroughPoll pins the layer the issue's
// "TTY and non-TTY request bodies carry the CSR" test targets: the shared poll
// closure in runDeviceAuthFlowWithCSR. go test's stdout is not a TTY, so the
// non-TTY branch runs for real against the mock; the TTY branch is covered by
// construction (both branches call the SAME closure).
func TestRunDeviceAuthFlowWithCSR_ThreadsCSRThroughPoll(t *testing.T) {
	mock := nexus.StartMockDeviceAuthServer(2) // one pending, then success
	defer mock.Close()

	res, err := runDeviceAuthFlowWithCSR(mock.URL(), false, testLoginCSR)
	if err != nil {
		t.Fatalf("runDeviceAuthFlowWithCSR: %v", err)
	}
	if res == nil || res.Token == nil || res.Token.Authkey == "" {
		t.Fatal("expected a token with an authkey")
	}
	csrs := mock.TokenRequestCSRs()
	if len(csrs) == 0 {
		t.Fatal("expected at least one token request")
	}
	for i, got := range csrs {
		if got != testLoginCSR {
			t.Errorf("poll %d csr_pem = %q, want the threaded CSR", i, got)
		}
	}
}

// TestRunDeviceAuthFlow_LegacyNoCSRInBody pins that the legacy entry point sends
// NO csr_pem (init/enroll/control-center byte-compat).
func TestRunDeviceAuthFlow_LegacyNoCSRInBody(t *testing.T) {
	mock := nexus.StartMockDeviceAuthServer(1)
	defer mock.Close()

	if _, err := runDeviceAuthFlow(mock.URL(), false); err != nil {
		t.Fatalf("runDeviceAuthFlow: %v", err)
	}
	for i, body := range mock.TokenRequestBodies() {
		if strings.Contains(body, "csr_pem") {
			t.Errorf("legacy poll %d unexpectedly carried csr_pem: %s", i, body)
		}
	}
}

// selfSignedLeafPEM builds a self-signed X.509 leaf whose SubjectPublicKey is
// key's public half. Our validation only checks that binding (the CA identity
// is irrelevant to persistLoginIdentityBundle), so a self-signed cert is a
// faithful stand-in for a real fabric CA leaf.
func selfSignedLeafPEM(t *testing.T, key *ecdsa.PrivateKey, serial int64) string {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "citadel-node"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	return k
}

// hermeticStore returns a nodeidentity.Store rooted at a temp dir (New => NO
// legacy read-through, so it never touches the real host identity).
func hermeticStore(t *testing.T) *nodeidentity.Store {
	t.Helper()
	return nodeidentity.New(filepath.Join(t.TempDir(), "identity"))
}

// TestGenerateLoginCSR_BoundToKeyAndSameAsSigner pins the K-A property: the
// login CSR key is the SAME key the receipt signer / enrollment paths resolve
// when they construct a store from the SAME converged node config dir, and the
// generated CSR is bound to exactly that key. Fully hermetic (New, temp dir).
func TestGenerateLoginCSR_BoundToKeyAndSameAsSigner(t *testing.T) {
	converged := t.TempDir() // stands in for network.GetNodeConfigDir()'s result
	identityDir := filepath.Join(converged, "identity")

	// Login CSR path.
	loginStore := nodeidentity.New(identityDir)
	csrPEM, pub, err := generateLoginCSR(loginStore)
	if err != nil {
		t.Fatalf("generateLoginCSR: %v", err)
	}

	// The CSR parses, self-verifies, and carries exactly the returned pubkey.
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		t.Fatalf("CSR is not a PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("CSR self-signature invalid: %v", err)
	}
	csrPub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || !csrPub.Equal(pub) {
		t.Fatalf("CSR public key does not match the returned login key")
	}

	// Signer path: a DIFFERENT Store instance built from the SAME converged
	// dir (exactly what internal/worker.defaultAEPSigner and cmd/init.go do)
	// must resolve the identical key -> identical fingerprint.
	loginFP, err := loginStore.PublicKeyFingerprint()
	if err != nil {
		t.Fatalf("login fingerprint: %v", err)
	}
	signerStore := nodeidentity.New(identityDir)
	signerFP, err := signerStore.PublicKeyFingerprint()
	if err != nil {
		t.Fatalf("signer fingerprint: %v", err)
	}
	if loginFP != signerFP {
		t.Fatalf("login CSR key != signer key (K-A broken): %q vs %q", loginFP, signerFP)
	}
}

// TestLoginIdentityStore_SameConvergentDirAsCSRPath pins the wiring: the login
// store is nodeidentity.Convergent(nodeConfigDirFn()), the SAME construction the
// AEP signer and CSR/enroll paths use. Compares resolved dirs only (no I/O), and
// swaps the nodeConfigDirFn seam so the real node dir is never touched.
func TestLoginIdentityStore_SameConvergentDirAsCSRPath(t *testing.T) {
	tmp := t.TempDir()
	orig := nodeConfigDirFn
	nodeConfigDirFn = func() string { return tmp }
	defer func() { nodeConfigDirFn = orig }()

	got := loginIdentityStore().Dir()
	want := nodeidentity.Convergent(tmp).Dir()
	if got != want {
		t.Fatalf("login store dir %q != convergent CSR path dir %q", got, want)
	}
}

// TestPersistLoginIdentityBundle_ValidBundlePersists: a well-formed bundle bound
// to the node key is stored, and the node_uid is surfaced for serving identity.
func TestPersistLoginIdentityBundle_ValidBundlePersists(t *testing.T) {
	store := hermeticStore(t)
	key, err := store.GetOrCreateKey()
	if err != nil {
		t.Fatalf("GetOrCreateKey: %v", err)
	}
	leaf := selfSignedLeafPEM(t, key, 1)
	chain := selfSignedLeafPEM(t, key, 2)

	out, err := persistLoginIdentityBundle(store, &key.PublicKey, leaf, chain, "uid-abc")
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if !out.Persisted || out.NodeUID != "uid-abc" {
		t.Fatalf("outcome = %+v, want Persisted=true NodeUID=uid-abc", out)
	}
	if !store.HasLeaf() {
		t.Fatal("expected leaf on disk")
	}
}

// TestPersistLoginIdentityBundle_MismatchedLeafRejected: a leaf bound to a
// DIFFERENT key is rejected and nothing is written (fail closed).
func TestPersistLoginIdentityBundle_MismatchedLeafRejected(t *testing.T) {
	store := hermeticStore(t)
	key, err := store.GetOrCreateKey()
	if err != nil {
		t.Fatalf("GetOrCreateKey: %v", err)
	}
	otherLeaf := selfSignedLeafPEM(t, newKey(t), 1) // bound to a stranger key

	out, err := persistLoginIdentityBundle(store, &key.PublicKey, otherLeaf, "", "uid-abc")
	if err == nil {
		t.Fatalf("expected rejection, got outcome %+v", out)
	}
	if store.HasLeaf() {
		t.Fatal("mismatched leaf must not be written")
	}
}

// TestPersistLoginIdentityBundle_PartialBundleRejected: leaf xor node_uid.
func TestPersistLoginIdentityBundle_PartialBundleRejected(t *testing.T) {
	store := hermeticStore(t)
	key, err := store.GetOrCreateKey()
	if err != nil {
		t.Fatalf("GetOrCreateKey: %v", err)
	}
	leaf := selfSignedLeafPEM(t, key, 1)

	// Leaf without node_uid.
	if _, err := persistLoginIdentityBundle(store, &key.PublicKey, leaf, "", ""); err == nil {
		t.Error("leaf without node_uid should be rejected")
	}
	// node_uid without leaf.
	if _, err := persistLoginIdentityBundle(store, &key.PublicKey, "", "", "uid-abc"); err == nil {
		t.Error("node_uid without leaf should be rejected")
	}
	if store.HasLeaf() {
		t.Fatal("partial bundle must not write a leaf")
	}
}

// TestPersistLoginIdentityBundle_MalformedNodeUIDRejected.
func TestPersistLoginIdentityBundle_MalformedNodeUIDRejected(t *testing.T) {
	store := hermeticStore(t)
	key, err := store.GetOrCreateKey()
	if err != nil {
		t.Fatalf("GetOrCreateKey: %v", err)
	}
	leaf := selfSignedLeafPEM(t, key, 1)

	for _, bad := range []string{"bad uid", "uid/../x", "a\nb", "toolong-" + string(make([]byte, 70))} {
		if _, err := persistLoginIdentityBundle(store, &key.PublicKey, leaf, "", bad); err == nil {
			t.Errorf("malformed node_uid %q should be rejected", bad)
		}
	}
	if store.HasLeaf() {
		t.Fatal("malformed node_uid must not write a leaf")
	}
}

// TestPersistLoginIdentityBundle_NoBundleNoOp: the universal inert path.
func TestPersistLoginIdentityBundle_NoBundleNoOp(t *testing.T) {
	store := hermeticStore(t)
	key, err := store.GetOrCreateKey()
	if err != nil {
		t.Fatalf("GetOrCreateKey: %v", err)
	}
	out, err := persistLoginIdentityBundle(store, &key.PublicKey, "", "", "")
	if err != nil {
		t.Fatalf("no-bundle should be a no-op, got %v", err)
	}
	if out.Persisted || out.NodeUID != "" {
		t.Fatalf("no-bundle outcome = %+v, want zero", out)
	}
	if store.HasLeaf() {
		t.Fatal("no-bundle must not write a leaf")
	}
}

// TestPersistLoginIdentityBundle_BundleWithoutCSRRejected: a bundle returned
// when this login submitted NO CSR (csrPub == nil) is refused outright.
func TestPersistLoginIdentityBundle_BundleWithoutCSRRejected(t *testing.T) {
	store := hermeticStore(t)
	key, err := store.GetOrCreateKey()
	if err != nil {
		t.Fatalf("GetOrCreateKey: %v", err)
	}
	leaf := selfSignedLeafPEM(t, key, 1)

	if _, err := persistLoginIdentityBundle(store, nil, leaf, "", "uid-abc"); err == nil {
		t.Fatal("bundle without a submitted CSR must be rejected")
	}
	if store.HasLeaf() {
		t.Fatal("must not write a leaf we never requested")
	}
}

// TestPersistLoginIdentityBundle_DoesNotOverwriteExisting: re-login idempotence
// — a known-good leaf on disk is never overwritten, and the serving identity
// still resolves.
func TestPersistLoginIdentityBundle_DoesNotOverwriteExisting(t *testing.T) {
	store := hermeticStore(t)
	key, err := store.GetOrCreateKey()
	if err != nil {
		t.Fatalf("GetOrCreateKey: %v", err)
	}
	first := selfSignedLeafPEM(t, key, 1)
	if _, err := persistLoginIdentityBundle(store, &key.PublicKey, first, "", "uid-abc"); err != nil {
		t.Fatalf("first persist: %v", err)
	}
	before, err := os.ReadFile(store.LeafPath())
	if err != nil {
		t.Fatalf("read leaf: %v", err)
	}

	// A DIFFERENT (still valid) leaf on a second login must not overwrite.
	second := selfSignedLeafPEM(t, key, 2)
	out, err := persistLoginIdentityBundle(store, &key.PublicKey, second, "", "uid-abc")
	if err != nil {
		t.Fatalf("second persist: %v", err)
	}
	if out.Persisted {
		t.Error("second persist must not report a new write")
	}
	if out.NodeUID != "uid-abc" {
		t.Errorf("NodeUID = %q, want uid-abc (serving identity still applies)", out.NodeUID)
	}
	after, err := os.ReadFile(store.LeafPath())
	if err != nil {
		t.Fatalf("read leaf: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("known-good leaf must not be overwritten on re-login")
	}
}

// TestPersistLoginIdentityBundle_ExistingMismatchedLeafRefused: a leaf already
// on disk that is NOT bound to our key is refused (not overwritten, no NodeUID),
// distinguishing a stale/foreign cert from idempotent re-login.
func TestPersistLoginIdentityBundle_ExistingMismatchedLeafRefused(t *testing.T) {
	store := hermeticStore(t)
	key, err := store.GetOrCreateKey()
	if err != nil {
		t.Fatalf("GetOrCreateKey: %v", err)
	}
	// Pre-seed a leaf bound to a STRANGER key via the store's own path.
	strangerLeaf := selfSignedLeafPEM(t, newKey(t), 1)
	if err := store.StoreLeaf(strangerLeaf, ""); err != nil {
		t.Fatalf("seed stranger leaf: %v", err)
	}

	valid := selfSignedLeafPEM(t, key, 2)
	out, err := persistLoginIdentityBundle(store, &key.PublicKey, valid, "", "uid-abc")
	if err == nil {
		t.Fatalf("expected refusal on mismatched existing leaf, got %+v", out)
	}
	if out.NodeUID != "" {
		t.Errorf("must not return a NodeUID when refusing: %+v", out)
	}
	// The stranger leaf is left untouched (never overwritten).
	after, err := os.ReadFile(store.LeafPath())
	if err != nil {
		t.Fatalf("read leaf: %v", err)
	}
	if string(after) != strangerLeaf {
		t.Fatal("existing leaf must not be overwritten on refusal")
	}
}

// TestServingIdentityHostname: node-{uid} when enrolled (and NOT the display
// hostname, the verifier-rejection case), display hostname otherwise, and
// deterministic across calls (re-login idempotence of the serving name).
func TestServingIdentityHostname(t *testing.T) {
	if got := servingIdentityHostname("", "my-laptop"); got != "my-laptop" {
		t.Errorf("no uid: got %q, want display hostname my-laptop", got)
	}
	got := servingIdentityHostname("abc123", "my-laptop")
	if got != "node-abc123" {
		t.Errorf("enrolled: got %q, want node-abc123", got)
	}
	if got == "my-laptop" {
		t.Error("enrolled serving identity must NOT be the display hostname (verifier would reject)")
	}
	if a, b := servingIdentityHostname("abc123", "my-laptop"), servingIdentityHostname("abc123", "renamed"); a != b {
		t.Errorf("serving name must be deterministic in node_uid, got %q vs %q", a, b)
	}
}
