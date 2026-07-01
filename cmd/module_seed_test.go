package cmd

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
)

// TestModuleSourcesFromCatalog covers the seeding mapper that turns the catalog
// registry + curated index into the TUI's known-modules picker rows: registry
// entries are trusted (Tier 0), index entries carry their source repo and are
// untrusted, and duplicate names de-duplicate with the registry winning.
func TestModuleSourcesFromCatalog(t *testing.T) {
	reg := &catalog.Registry{
		Version: 1,
		Services: []catalog.RegistryEntry{
			{Name: "vllm", Description: "vLLM GPU inference"},
			{Name: "", Description: "skip empty name"},
			{Name: "vllm", Description: "dupe within registry"},
		},
	}
	idx := &catalog.ModuleIndex{
		Version: 1,
		Modules: []catalog.ModuleIndexEntry{
			{Name: "cool-bot", Source: "owner/cool-bot", Description: "community bot"},
			{Name: "vllm", Source: "owner/vllm-fork", Description: "collides with registry"},
		},
	}

	got := moduleSourcesFromCatalog(reg, idx)

	if len(got) != 2 {
		t.Fatalf("moduleSourcesFromCatalog returned %d rows, want 2 (vllm + cool-bot): %+v", len(got), got)
	}

	// Registry entry: trusted, source == catalog name.
	if got[0].Name != "vllm" || got[0].Source != "vllm" || !got[0].Trusted {
		t.Errorf("row 0 = %+v, want {Name:vllm Source:vllm Trusted:true}", got[0])
	}
	if got[0].Description != "vLLM GPU inference" {
		t.Errorf("registry-winning dedupe failed: description = %q", got[0].Description)
	}

	// Index entry: untrusted, source == owner/repo.
	if got[1].Name != "cool-bot" || got[1].Source != "owner/cool-bot" || got[1].Trusted {
		t.Errorf("row 1 = %+v, want {Name:cool-bot Source:owner/cool-bot Trusted:false}", got[1])
	}
}

// TestModuleSourcesFromCatalogEmpty asserts the mapper returns no rows for nil
// inputs, which is the signal listModuleSources uses to substitute the fallback
// curated set (so the picker is never blank on a fresh, un-cloned node).
func TestModuleSourcesFromCatalogEmpty(t *testing.T) {
	if got := moduleSourcesFromCatalog(nil, nil); len(got) != 0 {
		t.Errorf("moduleSourcesFromCatalog(nil, nil) = %+v, want empty", got)
	}
	if len(fallbackModuleSources) == 0 {
		t.Error("fallbackModuleSources is empty; the picker would be blank on a fresh node")
	}
	for _, s := range fallbackModuleSources {
		if s.Name == "" || s.Source == "" {
			t.Errorf("fallback source has empty Name/Source: %+v", s)
		}
	}
}
