package worker

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/aep"
)

// v2ReceiptKeys are the 17 keys a SIGNED v2 AEP receipt map carries verbatim from
// ToMap() — the exact element shape central's NodeUsageRecord.receipts attests
// (aceteam#11146 / citadel-cli#1291). signature/public_key_fingerprint are present
// precisely because the receipt is signed (they are omitempty on the struct).
var v2ReceiptKeys = []string{
	"receipt_version", "node_id", "job_id", "issued_at", "engine", "model",
	"input_sha256", "output_sha256", "policy_hash", "action", "verdict_hash",
	"grounded", "score", "claims_checked", "flagged_hash",
	"signature", "public_key_fingerprint",
}

// signedWorkReceiptMap builds a real signed v2 work receipt with the in-memory
// test signer and returns its ToMap() form — the exact bytes a handler/runner
// puts in job output `work_receipts`.
func signedWorkReceiptMap(t *testing.T, s *goodSigner, jobID string) map[string]any {
	t.Helper()
	r, err := aep.BuildSignedWorkReceipt(s, "fabric-7", jobID, aep.WorkActionIndex, "sha256:aa", "sha256:bb", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatalf("BuildSignedWorkReceipt: %v", err)
	}
	m, err := r.ToMap()
	if err != nil {
		t.Fatalf("ToMap: %v", err)
	}
	return m
}

// assertReceiptShape17Keys pins that a receipts[] element is the v2 ToMap() shape:
// EXACTLY 17 keys, each the expected snake_case name (emit every key even when the
// value is empty, e.g. engine/model/verdict_hash empty strings on a work receipt).
func assertReceiptShape17Keys(t *testing.T, m map[string]any) {
	t.Helper()
	if len(m) != 17 {
		t.Fatalf("receipt should have exactly 17 keys (signed v2 ToMap shape), got %d: %v", len(m), m)
	}
	for _, k := range v2ReceiptKeys {
		if _, ok := m[k]; !ok {
			t.Errorf("receipt missing key %q: %v", k, m)
		}
	}
	// A work receipt's grounding/verdict fields are empty by construction.
	if m["engine"] != "" || m["model"] != "" || m["verdict_hash"] != "" {
		t.Errorf("work receipt should have empty engine/model/verdict_hash, got engine=%v model=%v verdict=%v", m["engine"], m["model"], m["verdict_hash"])
	}
	if m["grounded"] != false {
		t.Errorf("work receipt grounded should be false, got %v", m["grounded"])
	}
}

// verifyReceiptMapSignature re-derives the v2 canonical bytes from a receipts[]
// element (map -> JSON -> AEPReceiptV2) and checks its signature against the
// signer's public key — proving the receipt survived the collection intact. This
// mirrors the aep package's own verify path (sign over CanonicalizeV2, base64 DER
// signature).
func verifyReceiptMapSignature(t *testing.T, s *goodSigner, m map[string]any) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal receipt map: %v", err)
	}
	var r aep.AEPReceiptV2
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("unmarshal into AEPReceiptV2: %v", err)
	}
	sig, ok := m["signature"].(string)
	if !ok || sig == "" {
		t.Fatalf("receipt has no signature: %v", m)
	}
	sigDER, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	digest := sha256.Sum256(aep.CanonicalizeV2(&r))
	if !ecdsa.VerifyASN1(&s.key.PublicKey, digest[:], sigDER) {
		t.Fatal("receipt signature does not verify over CanonicalizeV2 after round-tripping through the record")
	}
}

// TestBuildUsageRecordCollectsNativeWorkReceipt pins the runner-signed (native,
// top-level `work_receipts`) path: buildUsageRecord carries the full signed
// receipt map onto the record, verbatim (17 keys) and still verifiable.
func TestBuildUsageRecordCollectsNativeWorkReceipt(t *testing.T) {
	s := newGoodSigner(t)
	m := signedWorkReceiptMap(t, s, "inf-1")
	result := &JobResult{Status: JobStatusSuccess, Output: map[string]any{
		"content":       "hi",
		"work_receipts": []map[string]any{m},
	}}

	rec := buildUsageRecord(&Job{ID: "inf-1", Type: "llm_inference"}, "success", time.Now(), time.Now(), result, nil)

	if len(rec.Receipts) != 1 {
		t.Fatalf("expected 1 receipt on the record, got %d", len(rec.Receipts))
	}
	if rec.ReceiptsTruncated != 0 {
		t.Errorf("ReceiptsTruncated = %d, want 0", rec.ReceiptsTruncated)
	}
	assertReceiptShape17Keys(t, rec.Receipts[0])
	verifyReceiptMapSignature(t, s, rec.Receipts[0])

	// The element equals ToMap() verbatim (the pinned contract: no wrapper).
	wantJSON, _ := json.Marshal(m)
	gotJSON, _ := json.Marshal(rec.Receipts[0])
	if string(wantJSON) != string(gotJSON) {
		t.Errorf("receipts[0] is not ToMap() verbatim:\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

// TestBuildUsageRecordCollectsLegacyWorkReceipt pins the legacy handler-signed
// (FILE_INDEX / embedding) path, where the whole handler output is a JSON string
// under Output["output"], so `work_receipts` is nested there.
func TestBuildUsageRecordCollectsLegacyWorkReceipt(t *testing.T) {
	s := newGoodSigner(t)
	m := signedWorkReceiptMap(t, s, "idx-1")
	// Emulate the LegacyHandlerAdapter: marshal the handler output to a string.
	handlerOut, err := json.Marshal(map[string]any{
		"files_indexed":        1,
		"work_manifest_sha256": "sha256:aa",
		"work_receipts":        []map[string]any{m},
		"usage":                map[string]any{"origin": "dispatched", "action": "index", "signed": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := &JobResult{Status: JobStatusSuccess, Output: map[string]any{"output": string(handlerOut)}}

	rec := buildUsageRecord(&Job{ID: "idx-1", Type: "FILE_INDEX"}, "success", time.Now(), time.Now(), result, nil)

	if len(rec.Receipts) != 1 {
		t.Fatalf("expected 1 receipt on the record (from the nested legacy output), got %d", len(rec.Receipts))
	}
	assertReceiptShape17Keys(t, rec.Receipts[0])
	verifyReceiptMapSignature(t, s, rec.Receipts[0])
	// The nested-usage attribution is still carried (unchanged pre-existing behavior).
	if rec.Origin != "dispatched" || rec.WorkAction != "index" || !rec.ReceiptSigned {
		t.Errorf("nested usage attribution lost: origin=%q action=%q signed=%v", rec.Origin, rec.WorkAction, rec.ReceiptSigned)
	}
}

// TestBuildUsageRecordOneRecordManyReceipts pins the one-record-per-job contract:
// a job that produced N>1 receipts yields a SINGLE record carrying all N (central
// upserts ON CONFLICT (node_id, job_id) DO NOTHING, so splitting would lose them).
func TestBuildUsageRecordOneRecordManyReceipts(t *testing.T) {
	s := newGoodSigner(t)
	receipts := []map[string]any{
		signedWorkReceiptMap(t, s, "idx-N"),
		signedWorkReceiptMap(t, s, "idx-N"),
		signedWorkReceiptMap(t, s, "idx-N"),
	}
	result := &JobResult{Status: JobStatusSuccess, Output: map[string]any{
		"work_receipts": receipts,
	}}

	rec := buildUsageRecord(&Job{ID: "idx-N", Type: "FILE_INDEX"}, "success", time.Now(), time.Now(), result, nil)

	if len(rec.Receipts) != 3 {
		t.Fatalf("expected all 3 receipts on one record, got %d", len(rec.Receipts))
	}
	if rec.ReceiptsTruncated != 0 {
		t.Errorf("ReceiptsTruncated = %d, want 0 (all fit)", rec.ReceiptsTruncated)
	}
	for i, m := range rec.Receipts {
		assertReceiptShape17Keys(t, m)
		verifyReceiptMapSignature(t, s, m)
		_ = i
	}
}

// TestBuildUsageRecordTruncatesOversizeReceipts pins the <1 MB bound: when the
// receipts would exceed the budget, the record carries what fits plus a nonzero
// ReceiptsTruncated count — never a silent drop.
func TestBuildUsageRecordTruncatesOversizeReceipts(t *testing.T) {
	// Each fat receipt is ~200 KB, so only a handful fit under the ~768 KB budget.
	big := strings.Repeat("x", 200*1024)
	var receipts []map[string]any
	const n = 10
	for i := 0; i < n; i++ {
		receipts = append(receipts, map[string]any{"receipt_version": "2", "blob": big})
	}
	result := &JobResult{Status: JobStatusSuccess, Output: map[string]any{
		"work_receipts": receipts,
	}}

	rec := buildUsageRecord(&Job{ID: "fat-1", Type: "FILE_INDEX"}, "success", time.Now(), time.Now(), result, nil)

	if len(rec.Receipts) == 0 {
		t.Fatal("should keep at least one receipt, kept none")
	}
	if len(rec.Receipts) >= n {
		t.Fatalf("expected some receipts dropped under the bound, kept all %d", len(rec.Receipts))
	}
	if rec.ReceiptsTruncated != n-len(rec.Receipts) {
		t.Errorf("ReceiptsTruncated = %d, want %d (never a silent drop)", rec.ReceiptsTruncated, n-len(rec.Receipts))
	}
	// The kept receipts must actually be under the budget.
	kept, _ := json.Marshal(rec.Receipts)
	if len(kept) > usageReceiptsMaxBytes {
		t.Errorf("kept receipts %d bytes exceed budget %d", len(kept), usageReceiptsMaxBytes)
	}
}

// TestBuildUsageRecordNoReceipts pins back-compat: a success with no
// `work_receipts` key (every pre-#1291 job) carries no receipts and no truncation.
func TestBuildUsageRecordNoReceipts(t *testing.T) {
	result := &JobResult{Status: JobStatusSuccess, Output: map[string]any{"content": "hi"}}
	rec := buildUsageRecord(&Job{ID: "plain-1", Type: "llm_inference"}, "success", time.Now(), time.Now(), result, nil)
	if len(rec.Receipts) != 0 || rec.ReceiptsTruncated != 0 {
		t.Errorf("a job with no work receipts should carry none, got %d receipts, truncated=%d", len(rec.Receipts), rec.ReceiptsTruncated)
	}
}
