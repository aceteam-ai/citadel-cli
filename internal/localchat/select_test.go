package localchat

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

func TestBuildChoices(t *testing.T) {
	engines := []status.LocalEngine{
		{Name: "bonsai", Port: 8210, Models: []string{"Bonsai-27B-Q1_0.gguf"}},
		{Name: "vllm", Port: 8201, Models: []string{"a", "b"}},
		{Name: "llamacpp", Port: 8200, Models: nil}, // running, no model loaded
	}

	got := BuildChoices(engines)
	want := []EngineChoice{
		{Engine: "bonsai", Model: "Bonsai-27B-Q1_0.gguf", Port: 8210},
		{Engine: "vllm", Model: "a", Port: 8201},
		{Engine: "vllm", Model: "b", Port: 8201},
		{Engine: "llamacpp", Model: "", Port: 8200},
	}

	if len(got) != len(want) {
		t.Fatalf("got %d choices, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("choice[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// The no-model engine must produce a usable label.
	if got[3].Label() == "" {
		t.Error("empty-model choice should still have a label")
	}
}

func TestBuildChoices_Empty(t *testing.T) {
	if got := BuildChoices(nil); len(got) != 0 {
		t.Errorf("expected no choices, got %+v", got)
	}
}
