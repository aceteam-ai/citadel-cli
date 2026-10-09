package services

import "testing"

func intPtr(n int) *int { return &n }

// TestOllamaMaxLoadedModelsEnv pins the pure #1209 injection decision: inject the
// OLLAMA_MAX_LOADED_MODELS=<n> entry ONLY for the ollama service with a non-nil,
// non-negative value; nil (unset) and non-ollama inject nothing; a negative value
// is a hard error (the ResolveBindAddr refuse-loudly posture).
func TestOllamaMaxLoadedModelsEnv(t *testing.T) {
	cases := []struct {
		name       string
		service    string
		maxLoaded  *int
		wantEntry  string
		wantInject bool
		wantErr    bool
	}{
		{"ollama set to 1", "ollama", intPtr(1), "OLLAMA_MAX_LOADED_MODELS=1", true, false},
		{"ollama explicit 0 injects", "ollama", intPtr(0), "OLLAMA_MAX_LOADED_MODELS=0", true, false},
		{"ollama set to 3", "ollama", intPtr(3), "OLLAMA_MAX_LOADED_MODELS=3", true, false},
		{"ollama unset (nil) injects nothing", "ollama", nil, "", false, false},
		{"ollama negative is an error", "ollama", intPtr(-1), "", false, true},
		{"non-ollama with value injects nothing", "vllm", intPtr(1), "", false, false},
		{"non-ollama nil injects nothing", "llamacpp", nil, "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, inject, err := OllamaMaxLoadedModelsEnv(tc.service, tc.maxLoaded)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got entry=%q inject=%v", entry, inject)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if inject != tc.wantInject {
				t.Errorf("inject = %v, want %v", inject, tc.wantInject)
			}
			if entry != tc.wantEntry {
				t.Errorf("entry = %q, want %q", entry, tc.wantEntry)
			}
		})
	}
}
