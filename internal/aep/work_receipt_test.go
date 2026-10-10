package aep

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBuildSignedWorkReceipt pins the work-receipt shape (aceteam#10876 C7) and
// that it signs+verifies through the SAME v2 canonical form every other v2
// receipt uses, so `citadel aep verify` and the deployed verifier (aceteam#9287)
// can recompute it. A work receipt is event-style (empty verdict/grounding) like
// app_deploy.
func TestBuildSignedWorkReceipt(t *testing.T) {
	signer := newFakeSigner(t)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

	input := "sha256:" + hexOf("index-manifest")
	output := "sha256:" + hexOf("index-output")

	r, err := BuildSignedWorkReceipt(signer, "node-1", "job-idx-1", WorkActionIndex, input, output, now)
	if err != nil {
		t.Fatalf("BuildSignedWorkReceipt: %v", err)
	}
	if r.ReceiptVersion != ReceiptVersionV2 {
		t.Errorf("receipt_version = %q, want %q", r.ReceiptVersion, ReceiptVersionV2)
	}
	if r.Action != WorkActionIndex {
		t.Errorf("action = %q, want %q", r.Action, WorkActionIndex)
	}
	if r.InputSHA256 != input || r.OutputSHA256 != output {
		t.Errorf("digests = (%q,%q), want (%q,%q)", r.InputSHA256, r.OutputSHA256, input, output)
	}
	if r.PolicyHash != EmptyPolicyHash {
		t.Errorf("policy_hash = %q, want EmptyPolicyHash", r.PolicyHash)
	}
	// A work event carries no engine/model/verdict/grounding.
	if r.Engine != "" || r.Model != "" || r.VerdictHash != "" {
		t.Errorf("engine/model/verdict must be empty for a work event, got (%q,%q,%q)", r.Engine, r.Model, r.VerdictHash)
	}
	if r.Grounded || r.Score != 0 || r.ClaimsChecked != 0 {
		t.Errorf("grounding fields must be zero for a work event, got grounded=%v score=%v claims=%d", r.Grounded, r.Score, r.ClaimsChecked)
	}
	if r.FlaggedHash == "" {
		t.Error("flagged_hash must be the empty-list hash, not empty")
	}
	if r.Signature == "" || r.PublicKeyFingerprint == "" {
		t.Fatal("receipt was not signed")
	}

	sig, err := base64.StdEncoding.DecodeString(r.Signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	digest := sha256.Sum256(CanonicalizeV2(r))
	if !ecdsa.VerifyASN1(&signer.key.PublicKey, digest[:], sig) {
		t.Fatal("signature does not verify over CanonicalizeV2")
	}

	// embed_served is accepted; an unknown/verdict action is rejected (fail
	// closed) so a caller cannot route a content-verdict action through the
	// event-style work path.
	if _, err := BuildSignedWorkReceipt(signer, "n", "j", WorkActionEmbedServed, input, output, now); err != nil {
		t.Errorf("embed_served should be a valid work action: %v", err)
	}
	if _, err := BuildSignedWorkReceipt(signer, "n", "j", "pass", input, output, now); err == nil {
		t.Error("expected a Trust Engine verdict action to be rejected as a work action")
	}
	// The newline refuse-to-sign guard still applies (inherited from BuildSignedReceiptV2).
	if _, err := BuildSignedWorkReceipt(signer, "n\ninjected", "j", WorkActionIndex, input, output, now); err == nil {
		t.Error("expected a newline-bearing node_id to be refused")
	}
}

// TestGoldenWorkReceiptV2 is the cross-repo golden for a work receipt: it asserts
// a work receipt is a STANDARD v2 receipt (same 15 canonical fields, pinned by
// work_fields.txt), rebuilds a fixed sample from the committed v2 key, and checks
// its canonical bytes against work_canonical.bin plus a sign->verify round-trip
// against the committed leaf. With -update-golden it regenerates the fixtures.
//
// Regenerate with: go test ./internal/aep -run TestGoldenWorkReceipt -update-golden
// (NOT -run Golden, which also rewrites the inference golden's randomized signature).
func TestGoldenWorkReceiptV2(t *testing.T) {
	dir := v2TestdataDir()

	const (
		workJobID    = "job-golden-work"
		workIssuedAt = "2026-10-10T12:00:00Z"
	)
	inputSHA := sha256Prefixed("golden-index-input-manifest")
	outputSHA := sha256Prefixed("golden-index-output-manifest")

	// Reuse the committed v2 key + leaf so no new key material is introduced and
	// the committed signature verifies against the existing leaf.
	key := loadGoldenKey(t, filepath.Join(dir, "key.pem"))
	signer := &fakeSigner{key: key}
	nodeID := mustFingerprint(t, signer)

	build := func() *AEPReceiptV2 {
		r, err := BuildSignedWorkReceipt(signer, nodeID, workJobID, WorkActionIndex, inputSHA, outputSHA, mustTime(t, workIssuedAt))
		if err != nil {
			t.Fatalf("BuildSignedWorkReceipt: %v", err)
		}
		return r
	}

	fieldsPath := filepath.Join(dir, "work_fields.txt")
	canonPath := filepath.Join(dir, "work_canonical.bin")
	receiptPath := filepath.Join(dir, "work_receipt.json")

	if *updateGolden {
		writeFixture(t, fieldsPath, []byte(strings.Join(V2CanonicalFieldNames(), "\n")+"\n"))
		r := build()
		writeFixture(t, canonPath, CanonicalizeV2(r))
		receiptJSON, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		writeFixture(t, receiptPath, append(receiptJSON, '\n'))
		t.Logf("regenerated work-receipt golden fixtures in %s", dir)
		return
	}

	// 1) A work receipt uses the standard v2 canonical field set.
	wantFields := strings.Join(V2CanonicalFieldNames(), "\n") + "\n"
	if got := string(readFixture(t, fieldsPath)); got != wantFields {
		t.Errorf("work_fields.txt mismatch:\n got %q\nwant %q\n(run -run TestGoldenWorkReceipt -update-golden if deliberate)", got, wantFields)
	}

	// 2) Canonical bytes reproduce the committed golden exactly.
	r := build()
	gotCanon := CanonicalizeV2(r)
	wantCanon := readFixture(t, canonPath)
	if string(gotCanon) != string(wantCanon) {
		t.Errorf("CanonicalizeV2(work) mismatch:\n got %q\nwant %q", gotCanon, wantCanon)
	}

	// 3) The committed receipt.json canonicalizes to the same bytes and its
	//    committed signature verifies against the committed leaf's key.
	var committed AEPReceiptV2
	if err := json.Unmarshal(readFixture(t, receiptPath), &committed); err != nil {
		t.Fatalf("unmarshal work_receipt.json: %v", err)
	}
	if committed.Action != WorkActionIndex {
		t.Errorf("committed action = %q, want %q", committed.Action, WorkActionIndex)
	}
	if string(CanonicalizeV2(&committed)) != string(wantCanon) {
		t.Error("committed work_receipt.json canonicalizes differently than work_canonical.bin")
	}
	leafKey := loadGoldenLeafPublicKey(t, filepath.Join(dir, "leaf.pem"))
	sigDER, err := base64.StdEncoding.DecodeString(committed.Signature)
	if err != nil {
		t.Fatalf("decode committed signature: %v", err)
	}
	digest := sha256.Sum256(CanonicalizeV2(&committed))
	if !ecdsa.VerifyASN1(leafKey, digest[:], sigDER) {
		t.Error("committed work-receipt signature does not verify against the committed leaf")
	}
	if committed.PublicKeyFingerprint != nodeID {
		t.Errorf("committed public_key_fingerprint = %q, want %q", committed.PublicKeyFingerprint, nodeID)
	}

	// 4) ToMap round-trips (the shape attached to job output).
	m, err := committed.ToMap()
	if err != nil {
		t.Fatalf("ToMap: %v", err)
	}
	if m["action"] != WorkActionIndex {
		t.Errorf("ToMap action = %v, want %q", m["action"], WorkActionIndex)
	}
}
