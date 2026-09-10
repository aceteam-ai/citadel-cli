package aep

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/trust"
)

// TestCanonicalizeV2_MatchesDeployedVerifierExample pins CanonicalizeV2 against
// the exact byte sequence the deployed aceteam verifier's
// TestCanonicalizeReceiptV2.test_matches_go_canonical_format asserts
// (python-backend/tests/test_aep_receipt_verify.py, issue #8253 S2 / #9287).
// Verified against the real canonicalize_receipt_v2 during development.
func TestCanonicalizeV2_MatchesDeployedVerifierExample(t *testing.T) {
	r := &AEPReceiptV2{
		ReceiptVersion: "2",
		NodeID:         "n1",
		JobID:          "j1",
		IssuedAt:       "2026-09-06T12:00:00Z",
		Engine:         "bonsai",
		Model:          "bonsai-27b",
		InputSHA256:    "sha256:in",
		OutputSHA256:   "sha256:out",
		PolicyHash:     "sha256:pol",
		Action:         "flag",
		VerdictHash:    "sha256:vh",
		Grounded:       true,
		Score:          0.5,
		ClaimsChecked:  2,
		FlaggedHash:    "deadbeef",
	}
	want := strings.Join([]string{
		"2", "n1", "j1", "2026-09-06T12:00:00Z", "bonsai", "bonsai-27b",
		"sha256:in", "sha256:out", "sha256:pol", "flag", "sha256:vh",
		"true", "0.500000", "2", "deadbeef",
	}, "\n")
	if got := string(CanonicalizeV2(r)); got != want {
		t.Errorf("CanonicalizeV2:\n got %q\nwant %q", got, want)
	}
}

// TestCanonicalizeV2_SignatureFieldsExcluded pins that Signature and
// PublicKeyFingerprint are NOT part of the canonical bytes (same rule as v1).
func TestCanonicalizeV2_SignatureFieldsExcluded(t *testing.T) {
	r := &AEPReceiptV2{ReceiptVersion: "2", NodeID: "n", JobID: "j", IssuedAt: "t", Engine: "e", Model: "m", InputSHA256: "sha256:i", OutputSHA256: "sha256:o", PolicyHash: "sha256:p", Action: "pass", VerdictHash: "sha256:v", FlaggedHash: "h"}
	before := CanonicalizeV2(r)
	r.Signature = "AAAA"
	r.PublicKeyFingerprint = "sha256:xyz"
	if after := CanonicalizeV2(r); !bytes.Equal(before, after) {
		t.Errorf("signature fields changed the canon:\n before %q\n after  %q", before, after)
	}
}

// TestV2CanonicalFieldsMatchTestdata is the zero-cost SST drift guard: the
// v2CanonicalFields order must equal internal/aep/testdata/v2/fields.txt, the
// same ratified list a follow-up aceteam PR asserts against its
// _CANONICAL_FIELDS_V2.
func TestV2CanonicalFieldsMatchTestdata(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "v2", "fields.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var want []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			want = append(want, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	got := V2CanonicalFieldNames()
	if len(got) != len(want) {
		t.Fatalf("field count: canon has %d, fields.txt has %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("field[%d] = %q, fields.txt = %q", i, got[i], want[i])
		}
	}
}

// TestEmptyPolicyHash pins the EmptyPolicyHash literal equal to its definition,
// "sha256:" + hex(sha256("{}")) — canonicalizer-independent (every JSON
// canonicalizer renders the empty object as the two bytes {}).
func TestEmptyPolicyHash(t *testing.T) {
	sum := sha256.Sum256([]byte("{}"))
	want := "sha256:" + string(hexLower(sum[:]))
	if EmptyPolicyHash != want {
		t.Errorf("EmptyPolicyHash = %q, want %q", EmptyPolicyHash, want)
	}
}

func hexLower(b []byte) []byte {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexdigits[c>>4]
		out[i*2+1] = hexdigits[c&0x0f]
	}
	return out
}

// TestBuildSignedReceiptV2_RefusesNewlineCollision is the refuse-to-sign guard:
// engine="bonsai\nx",model="y" and engine="bonsai",model="x\ny" canonicalize to
// the IDENTICAL bytes (the non-injectivity the guard closes), so a single
// signature would vouch for both. BuildSignedReceiptV2 must refuse to sign
// either; an ordinary value signs fine.
func TestBuildSignedReceiptV2_RefusesNewlineCollision(t *testing.T) {
	signer := newFakeSigner(t)
	inputs := V2Inputs{InputSHA256: "sha256:i", OutputSHA256: "sha256:o", PolicyHash: EmptyPolicyHash, Action: "pass", VerdictHash: "sha256:v"}
	now := time.Unix(0, 0)

	// First, demonstrate the collision on the (total) canon itself.
	a := &AEPReceiptV2{ReceiptVersion: "2", NodeID: "n", JobID: "j", IssuedAt: "t", Engine: "bonsai\nx", Model: "y", InputSHA256: "sha256:i", OutputSHA256: "sha256:o", PolicyHash: EmptyPolicyHash, Action: "pass", VerdictHash: "sha256:v", FlaggedHash: "h"}
	b := &AEPReceiptV2{ReceiptVersion: "2", NodeID: "n", JobID: "j", IssuedAt: "t", Engine: "bonsai", Model: "x\ny", InputSHA256: "sha256:i", OutputSHA256: "sha256:o", PolicyHash: EmptyPolicyHash, Action: "pass", VerdictHash: "sha256:v", FlaggedHash: "h"}
	if !bytes.Equal(CanonicalizeV2(a), CanonicalizeV2(b)) {
		t.Fatal("expected the two newline-shifted receipts to collide on the canon (the property the guard exists to close)")
	}

	// The guard refuses to SIGN either colliding input.
	if _, err := BuildSignedReceiptV2(signer, "n", "j", "bonsai\nx", "y", inputs, trust.GroundingResult{}, now); err == nil {
		t.Error("BuildSignedReceiptV2 signed a receipt with a newline in engine; want refusal")
	}
	if _, err := BuildSignedReceiptV2(signer, "n", "j", "bonsai", "x\ny", inputs, trust.GroundingResult{}, now); err == nil {
		t.Error("BuildSignedReceiptV2 signed a receipt with a newline in model; want refusal")
	}
	// A newline smuggled through a digest field is refused too (uniform check).
	bad := V2Inputs{InputSHA256: "sha256:i\nx", OutputSHA256: "sha256:o", PolicyHash: EmptyPolicyHash, Action: "pass", VerdictHash: "sha256:v"}
	if _, err := BuildSignedReceiptV2(signer, "n", "j", "bonsai", "m", bad, trust.GroundingResult{}, now); err == nil {
		t.Error("BuildSignedReceiptV2 signed a receipt with a newline in input_sha256; want refusal")
	}

	// An ordinary receipt signs, and its signature verifies over the v2 canon.
	receipt, err := BuildSignedReceiptV2(signer, "n", "j", "bonsai", "bonsai-27b", inputs, trust.GroundingResult{Grounded: true, Score: 1.0}, now)
	if err != nil {
		t.Fatalf("BuildSignedReceiptV2 refused an ordinary receipt: %v", err)
	}
	if receipt.ReceiptVersion != ReceiptVersionV2 {
		t.Errorf("receipt_version = %q, want %q", receipt.ReceiptVersion, ReceiptVersionV2)
	}
	sigDER, err := base64.StdEncoding.DecodeString(receipt.Signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	digest := sha256.Sum256(CanonicalizeV2(receipt))
	pub, ok := publicKeyOf(t, signer)
	if !ok {
		return
	}
	if !ecdsa.VerifyASN1(pub, digest[:], sigDER) {
		t.Error("signature does not verify over CanonicalizeV2(receipt)")
	}
}

// publicKeyOf extracts the *ecdsa.PublicKey behind a fakeSigner for signature
// verification in-test.
func publicKeyOf(t *testing.T, s Signer) (*ecdsa.PublicKey, bool) {
	t.Helper()
	fs, ok := s.(*fakeSigner)
	if !ok {
		return nil, false
	}
	return &fs.key.PublicKey, true
}

// TestReceiptV2ToMapShape pins that ToMap yields the snake_case wire keys,
// including receipt_version, and omits empty signature fields only via omitempty.
func TestReceiptV2ToMapShape(t *testing.T) {
	r := &AEPReceiptV2{ReceiptVersion: "2", NodeID: "n", JobID: "j", IssuedAt: "t", Engine: "e", Model: "m", InputSHA256: "sha256:i", OutputSHA256: "sha256:o", PolicyHash: "sha256:p", Action: "pass", VerdictHash: "sha256:v", Grounded: true, Score: 1, ClaimsChecked: 0, FlaggedHash: "h", Signature: "AAAA", PublicKeyFingerprint: "sha256:fp"}
	m, err := r.ToMap()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"receipt_version", "node_id", "input_sha256", "output_sha256", "policy_hash", "action", "verdict_hash", "flagged_hash", "signature", "public_key_fingerprint"} {
		if _, ok := m[k]; !ok {
			t.Errorf("ToMap missing key %q: %v", k, m)
		}
	}
	if m["receipt_version"] != "2" {
		t.Errorf(`m["receipt_version"] = %v, want the string "2"`, m["receipt_version"])
	}
}
