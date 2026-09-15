package aep

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/trust"
)

// TestCanonicalizeV3_InjectiveWhereV1Collides is the headline of citadel-cli
// #1021, pinned side by side: the two newline-shifted receipts that collide to
// IDENTICAL bytes under the v1 newline canon (aep.Canonicalize) produce DISTINCT
// bytes under the RFC 8785 JCS v3 canon. The v1 collision is the flaw; the v3
// difference is the fix (injective by construction).
func TestCanonicalizeV3_InjectiveWhereV1Collides(t *testing.T) {
	// The v1 collision pair (engine/model are the free-string fields; only the
	// nine v1 fields matter to Canonicalize).
	v1a := &AEPReceiptV1{NodeID: "n", JobID: "j", IssuedAt: "t", Engine: "bonsai\nx", Model: "y", Grounded: true, Score: 1, ClaimsChecked: 0, FlaggedHash: "h"}
	v1b := &AEPReceiptV1{NodeID: "n", JobID: "j", IssuedAt: "t", Engine: "bonsai", Model: "x\ny", Grounded: true, Score: 1, ClaimsChecked: 0, FlaggedHash: "h"}
	if !bytes.Equal(Canonicalize(v1a), Canonicalize(v1b)) {
		t.Fatal("expected the v1 newline canon to COLLIDE on the two receipts (the flaw #1021 fixes)")
	}

	v3a := &AEPReceiptV3{ReceiptVersion: "3", NodeID: "n", JobID: "j", IssuedAt: "t", Engine: "bonsai\nx", Model: "y", InputSHA256: "sha256:i", OutputSHA256: "sha256:o", PolicyHash: "sha256:p", Action: "pass", VerdictHash: "sha256:v", Grounded: true, Score: 1, ClaimsChecked: 0, FlaggedHash: "h"}
	v3b := &AEPReceiptV3{ReceiptVersion: "3", NodeID: "n", JobID: "j", IssuedAt: "t", Engine: "bonsai", Model: "x\ny", InputSHA256: "sha256:i", OutputSHA256: "sha256:o", PolicyHash: "sha256:p", Action: "pass", VerdictHash: "sha256:v", Grounded: true, Score: 1, ClaimsChecked: 0, FlaggedHash: "h"}
	ca, err := CanonicalizeV3(v3a)
	if err != nil {
		t.Fatalf("CanonicalizeV3(a): %v", err)
	}
	cb, err := CanonicalizeV3(v3b)
	if err != nil {
		t.Fatalf("CanonicalizeV3(b): %v", err)
	}
	if bytes.Equal(ca, cb) {
		t.Errorf("v3 canon must be injective; the two newline-shifted receipts collided:\n a=%q\n b=%q", ca, cb)
	}
	// JCS escapes the newline as \n inside the string, so it can never shift a
	// field boundary. Spot-check the escaping is present.
	if !strings.Contains(string(ca), `bonsai\nx`) {
		t.Errorf("v3 canon should escape the newline (\\n) inside the engine string: %q", ca)
	}
}

// TestCanonicalizeV3_SignatureFieldsExcluded pins R4: signature and
// public_key_fingerprint are stripped before canonicalization.
func TestCanonicalizeV3_SignatureFieldsExcluded(t *testing.T) {
	r := &AEPReceiptV3{ReceiptVersion: "3", NodeID: "n", JobID: "j", IssuedAt: "t", Engine: "e", Model: "m", InputSHA256: "sha256:i", OutputSHA256: "sha256:o", PolicyHash: "sha256:p", Action: "pass", VerdictHash: "sha256:v", FlaggedHash: "h"}
	before, err := CanonicalizeV3(r)
	if err != nil {
		t.Fatal(err)
	}
	r.Signature = "AAAA"
	r.PublicKeyFingerprint = "sha256:xyz"
	after, err := CanonicalizeV3(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("signature fields changed the v3 canon:\n before %q\n after  %q", before, after)
	}
	if strings.Contains(string(after), "signature") || strings.Contains(string(after), "public_key_fingerprint") {
		t.Errorf("v3 canon must not contain the signature fields: %q", after)
	}
}

// TestCanonicalizeV3_NumberTransportRobust pins P3: an integral score renders as
// a bare integer (JCS 1.0 == 1 by value), so the Redis/API JSON hop that erases
// Go's float-ness is harmless -- the exact hazard the v2 'f' 6 rule dodges does
// not exist under JCS.
func TestCanonicalizeV3_NumberTransportRobust(t *testing.T) {
	one := &AEPReceiptV3{ReceiptVersion: "3", NodeID: "n", JobID: "j", IssuedAt: "t", Engine: "e", Model: "m", InputSHA256: "sha256:i", OutputSHA256: "sha256:o", PolicyHash: "sha256:p", Action: "pass", VerdictHash: "sha256:v", Score: 1.0, FlaggedHash: "h"}
	c, err := CanonicalizeV3(one)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(c), `"score":1`) || strings.Contains(string(c), `"score":1.0`) {
		t.Errorf("integral score 1.0 should canonicalize as \"score\":1 (JCS value-based number): %q", c)
	}
	half := &AEPReceiptV3{ReceiptVersion: "3", NodeID: "n", JobID: "j", IssuedAt: "t", Engine: "e", Model: "m", InputSHA256: "sha256:i", OutputSHA256: "sha256:o", PolicyHash: "sha256:p", Action: "pass", VerdictHash: "sha256:v", Score: 0.5, FlaggedHash: "h"}
	c2, err := CanonicalizeV3(half)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(c2), `"score":0.5`) {
		t.Errorf("score 0.5 should canonicalize as \"score\":0.5: %q", c2)
	}
}

// TestBuildSignedReceiptV3_SignsAndVerifies pins the sign->canon->verify round
// trip and receipt_version "3".
func TestBuildSignedReceiptV3_SignsAndVerifies(t *testing.T) {
	signer := newFakeSigner(t)
	inputs := V3Inputs{InputSHA256: "sha256:i", OutputSHA256: "sha256:o", PolicyHash: EmptyPolicyHash, Action: "pass", VerdictHash: "sha256:v"}
	receipt, err := BuildSignedReceiptV3(signer, "n", "j", "bonsai", "bonsai-27b", inputs, trust.GroundingResult{Grounded: true, Score: 1.0}, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("BuildSignedReceiptV3: %v", err)
	}
	if receipt.ReceiptVersion != ReceiptVersionV3 {
		t.Errorf("receipt_version = %q, want %q", receipt.ReceiptVersion, ReceiptVersionV3)
	}
	if receipt.Signature == "" || receipt.PublicKeyFingerprint == "" {
		t.Fatal("a signed receipt must carry both signature and public_key_fingerprint")
	}
	sigDER, err := base64.StdEncoding.DecodeString(receipt.Signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	canon, err := CanonicalizeV3(receipt)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canon)
	pub, ok := publicKeyOf(t, signer)
	if !ok {
		return
	}
	if !ecdsa.VerifyASN1(pub, digest[:], sigDER) {
		t.Error("signature does not verify over CanonicalizeV3(receipt)")
	}
}

// TestBuildSignedReceiptV3_NewlineSignsWithoutCollision is the contrast with v2's
// refuse-to-sign guard: v3 SIGNS a receipt whose engine/model contain a newline
// (no guard needed), AND the two v2-colliding inputs sign to DISTINCT canons --
// injective by construction, so one signature can never vouch for both.
func TestBuildSignedReceiptV3_NewlineSignsWithoutCollision(t *testing.T) {
	signer := newFakeSigner(t)
	inputs := V3Inputs{InputSHA256: "sha256:i", OutputSHA256: "sha256:o", PolicyHash: EmptyPolicyHash, Action: "pass", VerdictHash: "sha256:v"}
	now := time.Unix(0, 0)

	a, err := BuildSignedReceiptV3(signer, "n", "j", "bonsai\nx", "y", inputs, trust.GroundingResult{Grounded: true, Score: 1}, now)
	if err != nil {
		t.Fatalf("v3 refused a newline in engine (it should not): %v", err)
	}
	b, err := BuildSignedReceiptV3(signer, "n", "j", "bonsai", "x\ny", inputs, trust.GroundingResult{Grounded: true, Score: 1}, now)
	if err != nil {
		t.Fatalf("v3 refused a newline in model (it should not): %v", err)
	}
	ca, err := CanonicalizeV3(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := CanonicalizeV3(b)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ca, cb) {
		t.Error("v3 canon collided on the two newline-shifted receipts; it must be injective")
	}
}

// TestBuildSignedReceiptV3_RefusesNonFiniteScore pins R3: NaN/Inf are rejected at
// emission (JSON/JCS cannot represent them), never signed into an unsigned-hole.
func TestBuildSignedReceiptV3_RefusesNonFiniteScore(t *testing.T) {
	signer := newFakeSigner(t)
	inputs := V3Inputs{InputSHA256: "sha256:i", OutputSHA256: "sha256:o", PolicyHash: EmptyPolicyHash, Action: "pass", VerdictHash: "sha256:v"}
	for _, score := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := BuildSignedReceiptV3(signer, "n", "j", "e", "m", inputs, trust.GroundingResult{Score: score}, time.Unix(0, 0)); err == nil {
			t.Errorf("BuildSignedReceiptV3 signed a receipt with non-finite score %v; want refusal", score)
		}
	}
}

// TestCanonicalizeJCS_IntegerDomain pins R1 at the exact boundary the design's
// §9.2 run showed Python rfc8785 enforcing: the I-JSON safe range is
// [-(2^53)+1, 2^53-1], so 2^53-1 is accepted and 2^53 ITSELF is rejected (not
// merely above it). Accepting 2^53 on the Go side would bake in the very
// cross-language divergence JCS exists to remove.
func TestCanonicalizeJCS_IntegerDomain(t *testing.T) {
	const maxSafe = int64(1) << 53 // 2^53 == 9007199254740992
	cases := []struct {
		name    string
		n       int64
		wantErr bool
	}{
		{"2^53-1 accepted", maxSafe - 1, false},
		{"2^53 rejected", maxSafe, true},
		{"-(2^53-1) accepted", -(maxSafe - 1), false},
		{"-(2^53) rejected", -maxSafe, true},
		{"zero accepted", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CanonicalizeJCS(map[string]any{"n": tc.n})
			if tc.wantErr && err == nil {
				t.Errorf("CanonicalizeJCS(%d) = nil error, want an out-of-domain error", tc.n)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("CanonicalizeJCS(%d) = %v, want no error", tc.n, err)
			}
		})
	}

	// A large FLOAT (not an integer literal) is fine: JCS numbers are IEEE
	// doubles by design, so only the INTEGER domain is bounded. 1.2e20 is a
	// representable double and renders in fixed notation.
	if _, err := CanonicalizeJCS(map[string]any{"n": 1.2e20}); err != nil {
		t.Errorf("CanonicalizeJCS(1.2e20 as float64) = %v, want no error (floats are not integer-domain-checked)", err)
	}
}

// TestVerdictHashV3_UsesSharedCanonicalizer pins R5: the v3 verdict_hash is
// computed over the SAME CanonicalizeJCS the receipt uses, applied to
// trust.VerdictHashPreimage (reused byte-identically from the v2 path -- only the
// canonicalizer changes).
func TestVerdictHashV3_UsesSharedCanonicalizer(t *testing.T) {
	grounding := trust.GroundingResult{Grounded: false, Score: 0.5, ClaimsChecked: 2}
	verdict := trust.BuildVerdict("in", "out", grounding, trust.DefaultDetectors(), EmptyPolicyHash)

	got, err := VerdictHashV3(verdict.Action, verdict.Checks, grounding, EmptyPolicyHash)
	if err != nil {
		t.Fatal(err)
	}

	// Recompute independently: JCS over the same preimage object, sha256, prefixed.
	canon, err := CanonicalizeJCS(trust.VerdictHashPreimage(verdict.Action, verdict.Checks, grounding, EmptyPolicyHash))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canon)
	want := "sha256:" + string(hexLower(sum[:]))
	if got != want {
		t.Errorf("VerdictHashV3 = %q, want %q (must use the shared JCS canonicalizer over the preimage)", got, want)
	}
}

// TestReceiptV3ToMapShape pins that ToMap yields the snake_case wire keys, with
// receipt_version the string "3".
func TestReceiptV3ToMapShape(t *testing.T) {
	r := &AEPReceiptV3{ReceiptVersion: "3", NodeID: "n", JobID: "j", IssuedAt: "t", Engine: "e", Model: "m", InputSHA256: "sha256:i", OutputSHA256: "sha256:o", PolicyHash: "sha256:p", Action: "pass", VerdictHash: "sha256:v", Grounded: true, Score: 1, ClaimsChecked: 0, FlaggedHash: "h", Signature: "AAAA", PublicKeyFingerprint: "sha256:fp"}
	m, err := r.ToMap()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"receipt_version", "node_id", "input_sha256", "output_sha256", "policy_hash", "action", "verdict_hash", "flagged_hash", "signature", "public_key_fingerprint"} {
		if _, ok := m[k]; !ok {
			t.Errorf("ToMap missing key %q: %v", k, m)
		}
	}
	if m["receipt_version"] != "3" {
		t.Errorf(`m["receipt_version"] = %v, want the string "3"`, m["receipt_version"])
	}
}
