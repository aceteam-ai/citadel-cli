package usage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tempDBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "usage_test.db")
}

func TestOpenStore(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
}

func TestOpenStoreCreatesFile(t *testing.T) {
	path := tempDBPath(t)
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("database file should exist after OpenStore")
	}
}

func TestInsertAndQueryUnsynced(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Second)
	record := UsageRecord{
		JobID:            "job-001",
		JobType:          "llm_inference",
		Backend:          "vllm",
		Model:            "meta-llama/Llama-2-7b",
		Status:           "success",
		StartedAt:        now,
		CompletedAt:      now.Add(3 * time.Second),
		DurationMs:       3000,
		PromptTokens:     128,
		CompletionTokens: 256,
		TotalTokens:      384,
		RequestBytes:     1024,
		ResponseBytes:    4096,
		NodeID:           "test-node",
	}

	if err := store.Insert(record); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	records, err := store.QueryUnsynced(10)
	if err != nil {
		t.Fatalf("QueryUnsynced: %v", err)
	}

	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}

	r := records[0]
	if r.JobID != "job-001" {
		t.Errorf("JobID = %q, want %q", r.JobID, "job-001")
	}
	if r.Backend != "vllm" {
		t.Errorf("Backend = %q, want %q", r.Backend, "vllm")
	}
	if r.DurationMs != 3000 {
		t.Errorf("DurationMs = %d, want 3000", r.DurationMs)
	}
	if r.PromptTokens != 128 {
		t.Errorf("PromptTokens = %d, want 128", r.PromptTokens)
	}
	if r.TotalTokens != 384 {
		t.Errorf("TotalTokens = %d, want 384", r.TotalTokens)
	}
	if r.NodeID != "test-node" {
		t.Errorf("NodeID = %q, want %q", r.NodeID, "test-node")
	}
	if r.ID == 0 {
		t.Error("ID should be set after insert")
	}
}

func TestInsertDuplicateIgnored(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	record := UsageRecord{
		JobID:       "dup-job",
		JobType:     "test",
		Status:      "success",
		StartedAt:   now,
		CompletedAt: now,
		DurationMs:  100,
		NodeID:      "node1",
	}

	if err := store.Insert(record); err != nil {
		t.Fatalf("first Insert: %v", err)
	}

	// Second insert with same job_id should not error
	if err := store.Insert(record); err != nil {
		t.Fatalf("duplicate Insert should not error: %v", err)
	}

	records, err := store.QueryUnsynced(10)
	if err != nil {
		t.Fatalf("QueryUnsynced: %v", err)
	}
	if len(records) != 1 {
		t.Errorf("expected 1 record after duplicate insert, got %d", len(records))
	}
}

func TestMarkSynced(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	for i, id := range []string{"a", "b", "c"} {
		if err := store.Insert(UsageRecord{
			JobID:       id,
			JobType:     "test",
			Status:      "success",
			StartedAt:   now,
			CompletedAt: now.Add(time.Duration(i) * time.Second),
			DurationMs:  int64(i * 1000),
			NodeID:      "node1",
		}); err != nil {
			t.Fatalf("Insert %s: %v", id, err)
		}
	}

	// Query all unsynced
	records, err := store.QueryUnsynced(10)
	if err != nil {
		t.Fatalf("QueryUnsynced: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("expected 3 unsynced, got %d", len(records))
	}

	// Mark first two as synced
	if err := store.MarkSynced([]int64{records[0].ID, records[1].ID}); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}

	// Only one should remain unsynced
	remaining, err := store.QueryUnsynced(10)
	if err != nil {
		t.Fatalf("QueryUnsynced after mark: %v", err)
	}
	if len(remaining) != 1 {
		t.Errorf("expected 1 remaining unsynced, got %d", len(remaining))
	}
	if remaining[0].JobID != "c" {
		t.Errorf("remaining JobID = %q, want %q", remaining[0].JobID, "c")
	}
}

func TestMarkSyncedEmpty(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	// Should not error with empty slice
	if err := store.MarkSynced(nil); err != nil {
		t.Fatalf("MarkSynced(nil): %v", err)
	}
	if err := store.MarkSynced([]int64{}); err != nil {
		t.Fatalf("MarkSynced([]): %v", err)
	}
}

func TestQueryUnsyncedLimit(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	for i := range 5 {
		if err := store.Insert(UsageRecord{
			JobID:       fmt.Sprintf("job-%d", i),
			JobType:     "test",
			Status:      "success",
			StartedAt:   now,
			CompletedAt: now,
			DurationMs:  100,
			NodeID:      "node1",
		}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	records, err := store.QueryUnsynced(2)
	if err != nil {
		t.Fatalf("QueryUnsynced: %v", err)
	}
	if len(records) != 2 {
		t.Errorf("expected 2 records with limit=2, got %d", len(records))
	}
}

// TestWorkRecordRoundTrip pins that the aceteam#10876 C7 work-attribution and
// typed-unit fields survive Insert -> QueryUnsynced.
func TestWorkRecordRoundTrip(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Second)
	rec := UsageRecord{
		JobID:                 "work-idx-1",
		JobType:               "FILE_INDEX",
		Status:                "success",
		StartedAt:             now,
		CompletedAt:           now.Add(time.Second),
		DurationMs:            1000,
		NodeID:                "node-1",
		Origin:                OriginDispatchedForTest,
		OrgID:                 "org_42",
		WorkAction:            "index",
		Units:                 map[string]int64{"files_indexed": 3, "chunks_upserted": 12, "bytes": 2048},
		ReceiptManifestSHA256: "sha256:deadbeef",
		ReceiptSigned:         true,
	}
	if err := store.Insert(rec); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := store.QueryUnsynced(10)
	if err != nil {
		t.Fatalf("QueryUnsynced: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 record, got %d", len(got))
	}
	r := got[0]
	if r.Origin != OriginDispatchedForTest || r.OrgID != "org_42" || r.WorkAction != "index" {
		t.Errorf("attribution = (%q,%q,%q)", r.Origin, r.OrgID, r.WorkAction)
	}
	if r.ReceiptManifestSHA256 != "sha256:deadbeef" || !r.ReceiptSigned {
		t.Errorf("receipt binding = (%q,%v)", r.ReceiptManifestSHA256, r.ReceiptSigned)
	}
	if r.Units["files_indexed"] != 3 || r.Units["chunks_upserted"] != 12 || r.Units["bytes"] != 2048 {
		t.Errorf("units = %v", r.Units)
	}
}

// OriginDispatchedForTest avoids importing the const name indirectly; it is the
// same literal the production code uses.
const OriginDispatchedForTest = "dispatched"

// TestSchemaV2MigrationPreservesV1Rows hand-builds a v1-shaped job_usage table
// (pre-C7, no work columns, user_version 0), reopens it through OpenStore, and
// asserts the v2 columns are added and the old row still reads — the node-side
// SQLite migration path (aceteam#10876 C7).
func TestSchemaV2MigrationPreservesV1Rows(t *testing.T) {
	path := tempDBPath(t)

	// Build a v1 DB by hand (the exact pre-C7 shape).
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	const v1Schema = `
CREATE TABLE job_usage (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id            TEXT NOT NULL UNIQUE,
    job_type          TEXT NOT NULL,
    backend           TEXT NOT NULL DEFAULT '',
    model             TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL,
    started_at        TEXT NOT NULL,
    completed_at      TEXT NOT NULL,
    duration_ms       INTEGER NOT NULL,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    request_bytes     INTEGER NOT NULL DEFAULT 0,
    response_bytes    INTEGER NOT NULL DEFAULT 0,
    error_message     TEXT NOT NULL DEFAULT '',
    node_id           TEXT NOT NULL DEFAULT '',
    synced            INTEGER NOT NULL DEFAULT 0,
    created_at        TEXT NOT NULL DEFAULT (datetime('now'))
);`
	if _, err := db.Exec(v1Schema); err != nil {
		t.Fatalf("create v1 schema: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO job_usage (job_id, job_type, status, started_at, completed_at, duration_ms, total_tokens, node_id)
		VALUES ('v1-row', 'llm_inference', 'success', ?, ?, 500, 42, 'old-node')`, now, now); err != nil {
		t.Fatalf("insert v1 row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v1 db: %v", err)
	}

	// Reopen through OpenStore: the migration runs.
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore (migrate): %v", err)
	}
	defer store.Close()

	cols, err := tableColumns(store.db, "job_usage")
	if err != nil {
		t.Fatalf("tableColumns: %v", err)
	}
	for _, c := range []string{"origin", "org_id", "work_action", "units_json", "receipt_manifest_sha256", "receipt_signed"} {
		if _, ok := cols[c]; !ok {
			t.Errorf("migrated DB missing column %q", c)
		}
	}
	var uv int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&uv); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if uv != schemaVersion {
		t.Errorf("user_version = %d, want %d", uv, schemaVersion)
	}

	// The old row still reads, with empty work fields.
	got, err := store.QueryUnsynced(10)
	if err != nil {
		t.Fatalf("QueryUnsynced: %v", err)
	}
	if len(got) != 1 || got[0].JobID != "v1-row" || got[0].TotalTokens != 42 {
		t.Fatalf("old row did not survive migration: %+v", got)
	}
	if got[0].Origin != "" || got[0].WorkAction != "" || got[0].ReceiptSigned {
		t.Errorf("old row should have empty work fields, got %+v", got[0])
	}

	// A new work record inserts cleanly into the migrated DB.
	if err := store.Insert(UsageRecord{
		JobID: "post-migrate", JobType: "FILE_INDEX", Status: "success",
		StartedAt: time.Now(), CompletedAt: time.Now(), DurationMs: 1,
		Origin: "dispatched", WorkAction: "index", Units: map[string]int64{"files_indexed": 1},
	}); err != nil {
		t.Fatalf("insert into migrated DB: %v", err)
	}
}

func TestInsertWithErrorMessage(t *testing.T) {
	store, err := OpenStore(tempDBPath(t))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	record := UsageRecord{
		JobID:        "fail-job",
		JobType:      "llm_inference",
		Status:       "failed",
		StartedAt:    now,
		CompletedAt:  now,
		DurationMs:   50,
		ErrorMessage: "out of memory",
		NodeID:       "node1",
	}

	if err := store.Insert(record); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	records, err := store.QueryUnsynced(10)
	if err != nil {
		t.Fatalf("QueryUnsynced: %v", err)
	}
	if records[0].ErrorMessage != "out of memory" {
		t.Errorf("ErrorMessage = %q, want %q", records[0].ErrorMessage, "out of memory")
	}
}
