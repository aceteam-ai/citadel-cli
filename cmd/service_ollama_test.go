package cmd

import (
	"testing"

	svcports "github.com/aceteam-ai/citadel-cli/services"
)

// TestOllamaMaxLoadedModelsEntries pins the #1209 cmd-side entry helpers that
// startService (strict) and the port-drift recreate (best-effort) use to inject
// OLLAMA_MAX_LOADED_MODELS at `docker compose up`. Both take the *int directly so
// this is hermetic -- it never reads the live node manifest via
// findAndReadManifest/network.GetNodeConfigDir.
func TestOllamaMaxLoadedModelsEntries(t *testing.T) {
	one := 1
	zero := 0
	neg := -1

	// Strict (startService): set -> entry; nil -> nil; non-ollama -> nil;
	// negative -> error the operator can act on.
	if got, err := ollamaMaxLoadedModelsEntriesStrict("ollama", &one); err != nil || len(got) != 1 || got[0] != svcports.EnvOllamaMaxLoadedModels+"=1" {
		t.Errorf("strict(ollama,1) = (%v,%v), want ([%s=1],nil)", got, err, svcports.EnvOllamaMaxLoadedModels)
	}
	if got, err := ollamaMaxLoadedModelsEntriesStrict("ollama", &zero); err != nil || len(got) != 1 || got[0] != svcports.EnvOllamaMaxLoadedModels+"=0" {
		t.Errorf("strict(ollama,0) = (%v,%v), want ([%s=0],nil)", got, err, svcports.EnvOllamaMaxLoadedModels)
	}
	if got, err := ollamaMaxLoadedModelsEntriesStrict("ollama", nil); err != nil || got != nil {
		t.Errorf("strict(ollama,nil) = (%v,%v), want (nil,nil)", got, err)
	}
	if got, err := ollamaMaxLoadedModelsEntriesStrict("vllm", &one); err != nil || got != nil {
		t.Errorf("strict(vllm,1) = (%v,%v), want (nil,nil)", got, err)
	}
	if _, err := ollamaMaxLoadedModelsEntriesStrict("ollama", &neg); err == nil {
		t.Errorf("strict(ollama,-1) expected an error, got nil")
	}

	// Best-effort (port-drift recreate): negative degrades to nil, not a panic.
	if got := ollamaMaxLoadedModelsEntries("ollama", &one); len(got) != 1 || got[0] != svcports.EnvOllamaMaxLoadedModels+"=1" {
		t.Errorf("best-effort(ollama,1) = %v, want [%s=1]", got, svcports.EnvOllamaMaxLoadedModels)
	}
	if got := ollamaMaxLoadedModelsEntries("ollama", &neg); got != nil {
		t.Errorf("best-effort(ollama,-1) = %v, want nil (logged and dropped)", got)
	}
	if got := ollamaMaxLoadedModelsEntries("ollama", nil); got != nil {
		t.Errorf("best-effort(ollama,nil) = %v, want nil", got)
	}
}
