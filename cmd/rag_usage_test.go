package cmd

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/rag"
	"github.com/aceteam-ai/citadel-cli/internal/usage"
)

// TestRecordLocalIndexUsageTo pins the "citadel rag index lands in the same
// ledger" acceptance (aceteam#10876 C7): a local index writes exactly one
// local_cli / index work record, stamped with the node id, org, manifest hash,
// and typed units, into the usage store — and two runs do NOT collide (unique
// job id). It uses an explicit t.TempDir() DB path, never the live node's.
func TestRecordLocalIndexUsageTo(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	res := rag.IndexResult{
		Model:              "gte-multilingual-base",
		FilesIndexed:       4,
		FilesRemoved:       1,
		ChunksUpserted:     20,
		ChunksEmbedded:     22,
		Status:             "completed",
		WorkManifestSHA256: "sha256:cafe",
	}

	if err := recordLocalIndexUsageTo(dbPath, res, "org_9/docs", "node-local", time.Now()); err != nil {
		t.Fatalf("first record: %v", err)
	}
	if err := recordLocalIndexUsageTo(dbPath, res, "org_9/docs", "node-local", time.Now()); err != nil {
		t.Fatalf("second record: %v", err)
	}

	store, err := usage.OpenStore(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	recs, err := store.QueryUnsynced(100)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	// Two runs => two distinct rows (unique job ids), not one collapsed by dedup.
	if len(recs) != 2 {
		t.Fatalf("expected 2 local index records, got %d", len(recs))
	}
	seen := map[string]bool{}
	for _, r := range recs {
		if r.Origin != usageLocalCLI {
			t.Errorf("origin = %q, want %q", r.Origin, usageLocalCLI)
		}
		if r.WorkAction != "index" || r.OrgID != "org_9" || r.NodeID != "node-local" {
			t.Errorf("record attribution wrong: action=%q org=%q node=%q", r.WorkAction, r.OrgID, r.NodeID)
		}
		if r.ReceiptManifestSHA256 != "sha256:cafe" {
			t.Errorf("manifest hash = %q, want sha256:cafe", r.ReceiptManifestSHA256)
		}
		if r.Units["files_indexed"] != 4 || r.Units["chunks_upserted"] != 20 {
			t.Errorf("units = %v", r.Units)
		}
		if seen[r.JobID] {
			t.Errorf("duplicate job id %q — local runs must be unique", r.JobID)
		}
		seen[r.JobID] = true
	}
}

// usageLocalCLI mirrors jobs.OriginLocalCLI without importing jobs into the test.
const usageLocalCLI = "local_cli"
