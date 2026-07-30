package cmd

import (
	"encoding/json"
	"testing"
)

// TestSearchJSONShape locks the stable `citadel search --json` wire contract the
// desktop client (#617/#618/#619/#570) consumes. If this test needs updating,
// the wrapper contract changed and consumers must be told.
func TestSearchJSONShape(t *testing.T) {
	out := searchOutputJSON{
		Query: "refund policy",
		Count: 1,
		Model: "gte-multilingual-base",
		Results: []searchResultJSON{
			{Path: "/home/u/docs/policy.md", ChunkIndex: 2, Snippet: "Refunds are issued within 30 days.", Score: 0.8123},
		},
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Decode into a generic map to assert exact field names exist at each level.
	var top map[string]any
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"query", "count", "model", "results"} {
		if _, ok := top[k]; !ok {
			t.Errorf("top-level JSON missing required key %q; got %v", k, top)
		}
	}
	results, ok := top["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("results must be a 1-element array, got %v", top["results"])
	}
	first, ok := results[0].(map[string]any)
	if !ok {
		t.Fatalf("result[0] must be an object, got %T", results[0])
	}
	for _, k := range []string{"path", "chunk_index", "snippet", "score"} {
		if _, ok := first[k]; !ok {
			t.Errorf("result object missing required key %q; got %v", k, first)
		}
	}
	// Spot-check value fidelity.
	if first["path"] != "/home/u/docs/policy.md" {
		t.Errorf("path field wrong: %v", first["path"])
	}
	if first["snippet"] != "Refunds are issued within 30 days." {
		t.Errorf("snippet field wrong: %v", first["snippet"])
	}
}
