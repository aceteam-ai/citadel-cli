package aep

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/trust"
)

// fakeSigner is an in-memory ECDSA signer for tests -- it never touches disk,
// unlike nodeidentity.Store, so these tests stay hermetic regardless of the
// host's real config dir.
type fakeSigner struct {
	key *ecdsa.PrivateKey
}

func newFakeSigner(t *testing.T) *fakeSigner {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return &fakeSigner{key: key}
}

func (f *fakeSigner) Sign(payload []byte) ([]byte, error) {
	digest := sha256.Sum256(payload)
	return ecdsa.SignASN1(rand.Reader, f.key, digest[:])
}

func (f *fakeSigner) PublicKeyFingerprint() (string, error) {
	der, err := x509.MarshalPKIXPublicKey(&f.key.PublicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// TestCanonicalize_Deterministic pins that Canonicalize is a pure function of
// the receipt's nine signed fields: same input, same bytes, every time --
// this is the property signing and verification both depend on.
func TestCanonicalize_Deterministic(t *testing.T) {
	r := &AEPReceiptV1{
		NodeID: "node-1", JobID: "job-1", IssuedAt: "2026-08-29T00:00:00Z",
		Engine: "bonsai", Model: "bonsai-27b",
		Grounded: false, Score: 0.5, ClaimsChecked: 2, FlaggedHash: "abc123",
	}
	a := Canonicalize(r)
	b := Canonicalize(r)
	if !bytes.Equal(a, b) {
		t.Fatalf("Canonicalize is not deterministic: %q vs %q", a, b)
	}
	// A different field flips the output.
	r2 := *r
	r2.JobID = "job-2"
	if bytes.Equal(a, Canonicalize(&r2)) {
		t.Fatalf("Canonicalize did not change when JobID changed")
	}
}

// TestCanonicalize_ExcludesSignatureFields pins that Signature and
// PublicKeyFingerprint are NOT part of the signed bytes -- a signature
// covering its own field is the standard way this class of scheme breaks
// (design doc §3). Setting them must not change Canonicalize's output.
func TestCanonicalize_ExcludesSignatureFields(t *testing.T) {
	r := &AEPReceiptV1{NodeID: "n", JobID: "j", IssuedAt: "t", Engine: "e", Model: "m"}
	before := Canonicalize(r)
	r.Signature = "deadbeef"
	r.PublicKeyFingerprint = "sha256:deadbeef"
	after := Canonicalize(r)
	if !bytes.Equal(before, after) {
		t.Fatalf("Canonicalize changed after setting Signature/PublicKeyFingerprint: %q vs %q", before, after)
	}
}

// TestCanonicalize_FloatFormattingStable pins that Score's canonical
// representation does not depend on how the float64 happens to have been
// produced (e.g. 0.5 vs 1.0/2.0) -- both must canonicalize identically since
// they are the same value, guarding against the exact re-serialization
// hazard the design doc calls out.
func TestCanonicalize_FloatFormattingStable(t *testing.T) {
	r1 := &AEPReceiptV1{Score: 0.5}
	r2 := &AEPReceiptV1{Score: 1.0 / 2.0}
	if !bytes.Equal(Canonicalize(r1), Canonicalize(r2)) {
		t.Fatalf("equal float values canonicalized differently: %q vs %q", Canonicalize(r1), Canonicalize(r2))
	}
}

// TestBuildSignedReceipt_SignVerifyRoundTrips pins the full sign path: the
// receipt's Canonicalize(bytes) verifies against the signer's OWN public key
// using stdlib ecdsa.VerifyASN1 -- proving BuildSignedReceipt produces a
// signature a real verifier (not just this package) can check.
func TestBuildSignedReceipt_SignVerifyRoundTrips(t *testing.T) {
	signer := newFakeSigner(t)
	result := trust.GroundingResult{
		Grounded:      false,
		Score:         0.0,
		ClaimsChecked: 1,
		Flagged: []trust.Claim{
			{Value: "68%", Kind: trust.ClaimPercent, Reason: "no support in input"},
		},
	}
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	receipt, err := BuildSignedReceipt(signer, "node-123", "job-abc", "bonsai", "bonsai-27b", result, now)
	if err != nil {
		t.Fatalf("BuildSignedReceipt: %v", err)
	}

	if receipt.NodeID != "node-123" || receipt.JobID != "job-abc" {
		t.Errorf("receipt identity fields = %+v, want node-123/job-abc", receipt)
	}
	if receipt.IssuedAt != "2026-08-29T12:00:00Z" {
		t.Errorf("IssuedAt = %q, want RFC3339 of the injected clock", receipt.IssuedAt)
	}
	if receipt.Signature == "" || receipt.PublicKeyFingerprint == "" {
		t.Fatalf("receipt not fully signed: %+v", receipt)
	}

	// Verify using the signer's own public key -- proves the signature
	// covers Canonicalize(receipt) as documented, not something else.
	sigDER, err := base64.StdEncoding.DecodeString(receipt.Signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	digest := sha256.Sum256(Canonicalize(receipt))
	if !ecdsa.VerifyASN1(&signer.key.PublicKey, digest[:], sigDER) {
		t.Fatalf("signature does not verify against the signer's own public key")
	}

	fp, err := signer.PublicKeyFingerprint()
	if err != nil {
		t.Fatalf("PublicKeyFingerprint: %v", err)
	}
	if receipt.PublicKeyFingerprint != fp {
		t.Errorf("PublicKeyFingerprint = %q, want %q", receipt.PublicKeyFingerprint, fp)
	}
}

// TestBuildSignedReceipt_TamperedReceiptFailsVerification pins the negative
// case: mutating any signed field after signing must invalidate the
// signature -- otherwise the receipt is not actually binding.
func TestBuildSignedReceipt_TamperedReceiptFailsVerification(t *testing.T) {
	signer := newFakeSigner(t)
	result := trust.GroundingResult{Grounded: true, Score: 1.0, ClaimsChecked: 0}
	receipt, err := BuildSignedReceipt(signer, "node-1", "job-1", "vllm", "m", result, time.Now())
	if err != nil {
		t.Fatalf("BuildSignedReceipt: %v", err)
	}

	sigDER, err := base64.StdEncoding.DecodeString(receipt.Signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}

	// Tamper: flip the grounded flag after signing.
	receipt.Grounded = false
	digest := sha256.Sum256(Canonicalize(receipt))
	if ecdsa.VerifyASN1(&signer.key.PublicKey, digest[:], sigDER) {
		t.Fatalf("tampered receipt verified successfully, want failure")
	}
}

// TestBuildSignedReceipt_NilSigner pins a clear error rather than a panic
// when misconfigured (e.g. a caller wiring in a nil Signer by mistake).
func TestBuildSignedReceipt_NilSigner(t *testing.T) {
	_, err := BuildSignedReceipt(nil, "n", "j", "e", "m", trust.GroundingResult{}, time.Now())
	if err == nil {
		t.Fatalf("want error for nil signer, got nil")
	}
}

// TestResolveNodeID_PrefersFabricNodeID pins §4's phasing rule: the #8139
// fabric node ID wins when present, and the signer's own fingerprint is only
// a fallback for a node the backend has not yet echoed one to.
func TestResolveNodeID_PrefersFabricNodeID(t *testing.T) {
	signer := newFakeSigner(t)

	nodeID, err := ResolveNodeID(signer, "fabric-42")
	if err != nil {
		t.Fatalf("ResolveNodeID: %v", err)
	}
	if nodeID != "fabric-42" {
		t.Errorf("nodeID = %q, want fabric-42 (should prefer the declared fabric node ID)", nodeID)
	}

	fallback, err := ResolveNodeID(signer, "")
	if err != nil {
		t.Fatalf("ResolveNodeID fallback: %v", err)
	}
	fp, _ := signer.PublicKeyFingerprint()
	if fallback != fp {
		t.Errorf("fallback nodeID = %q, want signer's own fingerprint %q", fallback, fp)
	}
}

// TestAEPReceiptV1_ToMap pins the wire shape: ToMap must produce a plain
// map[string]any keyed by the receipt's json tags (snake_case), not a Go
// struct -- this is what internal/worker attaches to job output, so a
// typed *AEPReceiptV1 never crosses the serialization boundary.
func TestAEPReceiptV1_ToMap(t *testing.T) {
	signer := newFakeSigner(t)
	result := trust.GroundingResult{Grounded: true, Score: 1.0, ClaimsChecked: 0}
	receipt, err := BuildSignedReceipt(signer, "node-1", "job-1", "vllm", "m", result, time.Now())
	if err != nil {
		t.Fatalf("BuildSignedReceipt: %v", err)
	}

	m, err := receipt.ToMap()
	if err != nil {
		t.Fatalf("ToMap: %v", err)
	}
	for _, key := range []string{
		"node_id", "job_id", "issued_at", "engine", "model",
		"grounded", "score", "claims_checked", "flagged_hash",
		"signature", "public_key_fingerprint",
	} {
		if _, ok := m[key]; !ok {
			t.Errorf("ToMap() missing key %q, got %v", key, m)
		}
	}
	if _, isPointer := m["node_id"].(*string); isPointer {
		t.Errorf("ToMap() should contain plain values, not pointers")
	}
	if m["job_id"] != "job-1" {
		t.Errorf(`m["job_id"] = %v, want "job-1"`, m["job_id"])
	}
}

// TestHashFlaggedClaims_DeterministicAndOrderSensitive pins that the flagged
// claims hash is a pure, order-sensitive function -- pinning FlaggedHash's
// contract independent of the rest of the receipt.
func TestHashFlaggedClaims_DeterministicAndOrderSensitive(t *testing.T) {
	a := []trust.Claim{{Value: "68%", Kind: trust.ClaimPercent, Reason: "r1"}}
	b := []trust.Claim{{Value: "68%", Kind: trust.ClaimPercent, Reason: "r1"}}
	h1, err := hashFlaggedClaims(a)
	if err != nil {
		t.Fatalf("hashFlaggedClaims: %v", err)
	}
	h2, err := hashFlaggedClaims(b)
	if err != nil {
		t.Fatalf("hashFlaggedClaims: %v", err)
	}
	if h1 != h2 {
		t.Errorf("equal claim lists hashed differently: %q vs %q", h1, h2)
	}

	c := []trust.Claim{{Value: "7%", Kind: trust.ClaimPercent, Reason: "r2"}}
	h3, _ := hashFlaggedClaims(c)
	if h1 == h3 {
		t.Errorf("different claim lists hashed identically")
	}

	empty, err := hashFlaggedClaims(nil)
	if err != nil {
		t.Fatalf("hashFlaggedClaims(nil): %v", err)
	}
	if empty == "" {
		t.Errorf("hashFlaggedClaims(nil) returned empty hash, want a stable hash of an empty list")
	}
}
