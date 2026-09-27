package aep

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"testing"
	"time"
)

// TestBuildSignedAppDeployReceipt pins the app_deploy receipt shape (CRAM A1)
// and that it signs+verifies through the SAME v2 canonical form the inference
// path uses, so the deployed verifier (aceteam#9287, A4/A5) can recompute it.
func TestBuildSignedAppDeployReceipt(t *testing.T) {
	signer := newFakeSigner(t)
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

	input := "sha256:" + hexOf("manifest")
	output := "sha256:" + hexOf("digest\npod")

	r, err := BuildSignedAppDeployReceipt(signer, "node-1", "job-1", input, output, now)
	if err != nil {
		t.Fatalf("BuildSignedAppDeployReceipt: %v", err)
	}

	if r.ReceiptVersion != ReceiptVersionV2 {
		t.Errorf("receipt_version = %q, want %q", r.ReceiptVersion, ReceiptVersionV2)
	}
	if r.Action != AppDeployAction {
		t.Errorf("action = %q, want %q", r.Action, AppDeployAction)
	}
	if r.InputSHA256 != input || r.OutputSHA256 != output {
		t.Errorf("digests = (%q,%q), want (%q,%q)", r.InputSHA256, r.OutputSHA256, input, output)
	}
	if r.PolicyHash != EmptyPolicyHash {
		t.Errorf("policy_hash = %q, want EmptyPolicyHash", r.PolicyHash)
	}
	// A deploy carries no engine/model/verdict/grounding.
	if r.Engine != "" || r.Model != "" || r.VerdictHash != "" {
		t.Errorf("engine/model/verdict must be empty for a deploy, got (%q,%q,%q)", r.Engine, r.Model, r.VerdictHash)
	}
	if r.Grounded || r.Score != 0 || r.ClaimsChecked != 0 {
		t.Errorf("grounding fields must be zero for a deploy, got grounded=%v score=%v claims=%d", r.Grounded, r.Score, r.ClaimsChecked)
	}
	// FlaggedHash is the hash of the EMPTY flagged list, never "" (a verifier
	// recomputes hashFlaggedClaims(nil), not the empty string).
	if r.FlaggedHash == "" {
		t.Error("flagged_hash must be the empty-list hash, not empty")
	}
	if r.Signature == "" || r.PublicKeyFingerprint == "" {
		t.Fatal("receipt was not signed")
	}

	// The signature must verify over CanonicalizeV2 (the exact form the Python
	// verifier recomputes).
	sig, err := base64.StdEncoding.DecodeString(r.Signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	digest := sha256.Sum256(CanonicalizeV2(r))
	if !ecdsa.VerifyASN1(&signer.key.PublicKey, digest[:], sig) {
		t.Fatal("signature does not verify over CanonicalizeV2")
	}
}

func hexOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(sum)*2)
	for i, b := range sum {
		out[i*2] = hexdigits[b>>4]
		out[i*2+1] = hexdigits[b&0x0f]
	}
	return string(out)
}
