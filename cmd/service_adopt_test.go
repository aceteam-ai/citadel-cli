package cmd

import (
	"context"
	"strings"
	"testing"
)

// TestMaybeAdoptExternalEngineOnStart pins the aceteam-ai/citadel-cli#1081
// boot-time / `citadel run` adoption decision (startService's skip-launch gate),
// hermetic via injected seams -- no container runtime, no live engine.
func TestMaybeAdoptExternalEngineOnStart(t *testing.T) {
	serving := func(models []string, ok bool) func(context.Context, string, int) ([]string, bool) {
		return func(context.Context, string, int) ([]string, bool) { return models, ok }
	}

	t.Run("adopts when external serving and our container not running", func(t *testing.T) {
		msg, adopt := maybeAdoptExternalEngineOnStart(
			context.Background(), "vllm", true,
			func() bool { return false },
			serving([]string{"Qwen/Qwen3-8B"}, true),
		)
		if !adopt {
			t.Fatal("expected adopt=true")
		}
		if !strings.Contains(msg, "vllm") || !strings.Contains(msg, "Qwen/Qwen3-8B") {
			t.Errorf("message = %q, want engine + served model", msg)
		}
	})

	t.Run("does not adopt when nothing serving", func(t *testing.T) {
		if _, adopt := maybeAdoptExternalEngineOnStart(
			context.Background(), "vllm", true,
			func() bool { return false },
			serving(nil, false),
		); adopt {
			t.Fatal("expected adopt=false")
		}
	})

	t.Run("does not adopt when our container is running", func(t *testing.T) {
		if _, adopt := maybeAdoptExternalEngineOnStart(
			context.Background(), "vllm", true,
			func() bool { return true },
			serving([]string{"Qwen/Qwen3-8B"}, true),
		); adopt {
			t.Fatal("expected adopt=false when our own container is running")
		}
	})

	t.Run("does not adopt when runtime unavailable", func(t *testing.T) {
		probed := false
		if _, adopt := maybeAdoptExternalEngineOnStart(
			context.Background(), "vllm", false,
			func() bool { return false },
			func(context.Context, string, int) ([]string, bool) { probed = true; return nil, false },
		); adopt {
			t.Fatal("expected adopt=false when the container runtime is unavailable")
		}
		if probed {
			t.Error("must not probe the port when the runtime is unavailable")
		}
	})

	t.Run("does not adopt a non-allowlisted engine", func(t *testing.T) {
		probed := false
		if _, adopt := maybeAdoptExternalEngineOnStart(
			context.Background(), "ollama", true,
			func() bool { return false },
			func(context.Context, string, int) ([]string, bool) { probed = true; return []string{"llama3"}, true },
		); adopt {
			t.Fatal("expected adopt=false for a non-allowlisted engine")
		}
		if probed {
			t.Error("must not probe the port for a non-allowlisted engine")
		}
	})
}
