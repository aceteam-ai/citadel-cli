// cmd/work_usage_receipts_test.go
package cmd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/usage"
)

// TestUsageRecordPayloadCarriesReceipts pins that usageRecordPayload threads the
// signed work-receipt bodies (aceteam#11146 / citadel-cli#1291) onto the wire
// record as `receipts` + `receiptsTruncated`, and that the full stream entry
// serializes with the bumped payload version. The receipts[] elements are the
// receipt maps verbatim (no wrapper).
func TestUsageRecordPayloadCarriesReceipts(t *testing.T) {
	receipts := []map[string]any{
		{"receipt_version": "2", "job_id": "idx-1", "action": "index", "signature": "AAAA", "public_key_fingerprint": "sha256:fp"},
		{"receipt_version": "2", "job_id": "idx-1", "action": "index", "signature": "BBBB", "public_key_fingerprint": "sha256:fp"},
	}
	rec := usage.UsageRecord{
		JobID:             "idx-1",
		JobType:           "FILE_INDEX",
		Status:            "success",
		StartedAt:         time.Unix(1700000000, 0),
		CompletedAt:       time.Unix(1700000001, 0),
		NodeID:            "node-1",
		Receipts:          receipts,
		ReceiptsTruncated: 2,
	}

	p := usageRecordPayload(rec)
	if len(p.Receipts) != 2 || p.ReceiptsTruncated != 2 {
		t.Fatalf("payload receipts=%d truncated=%d, want 2/2", len(p.Receipts), p.ReceiptsTruncated)
	}

	entry := usageStreamEntry{Version: usage.PayloadVersion, NodeID: rec.NodeID, Record: p}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	var decoded struct {
		Version string `json:"version"`
		Record  struct {
			Receipts          []map[string]any `json:"receipts"`
			ReceiptsTruncated int              `json:"receiptsTruncated"`
		} `json:"record"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal entry: %v", err)
	}
	if decoded.Version != "4.0" {
		t.Errorf("payload version = %q, want 4.0", decoded.Version)
	}
	if len(decoded.Record.Receipts) != 2 {
		t.Fatalf("wire receipts = %d, want 2", len(decoded.Record.Receipts))
	}
	if decoded.Record.ReceiptsTruncated != 2 {
		t.Errorf("wire receiptsTruncated = %d, want 2", decoded.Record.ReceiptsTruncated)
	}
	// Elements are verbatim (no wrapper): the first receipt's own keys survive.
	if decoded.Record.Receipts[0]["signature"] != "AAAA" || decoded.Record.Receipts[0]["action"] != "index" {
		t.Errorf("receipt element not carried verbatim: %v", decoded.Record.Receipts[0])
	}
}

// TestUsageRecordPayloadOmitsReceiptsWhenAbsent pins back-compat: a record with no
// receipts emits neither `receipts` nor `receiptsTruncated` on the wire (omitempty),
// so a pre-#1291 record is byte-identical.
func TestUsageRecordPayloadOmitsReceiptsWhenAbsent(t *testing.T) {
	rec := usage.UsageRecord{
		JobID:       "plain-1",
		JobType:     "llm_inference",
		Status:      "success",
		StartedAt:   time.Unix(1700000000, 0),
		CompletedAt: time.Unix(1700000001, 0),
		NodeID:      "node-1",
	}
	raw, err := json.Marshal(usageRecordPayload(rec))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["receipts"]; ok {
		t.Errorf("receipts should be omitted when absent, got %v", m["receipts"])
	}
	if _, ok := m["receiptsTruncated"]; ok {
		t.Errorf("receiptsTruncated should be omitted when zero, got %v", m["receiptsTruncated"])
	}
}
