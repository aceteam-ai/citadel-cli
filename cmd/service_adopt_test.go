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

	t.Run("docker-less box (runtime unavailable) with external serving ADOPTS (#1084)", func(t *testing.T) {
		// The RM-01 shape: no container runtime, but a vendor vLLM serving. Post-#1084
		// this ADOPTS -- a runtime-less box cannot be running our container, so
		// ownership is NOT consulted (its func would panic if called) and the probe
		// decides.
		ownershipCalled := false
		msg, adopt := maybeAdoptExternalEngineOnStart(
			context.Background(), "vllm", false,
			func() bool { ownershipCalled = true; return false },
			serving([]string{"Qwen/Qwen3-8B"}, true),
		)
		if !adopt {
			t.Fatal("expected adopt=true on a docker-less box with an external engine serving (#1084)")
		}
		if ownershipCalled {
			t.Error("must not consult ourContainerRunning when the runtime is unavailable")
		}
		if !strings.Contains(msg, "vllm") || !strings.Contains(msg, "Qwen/Qwen3-8B") {
			t.Errorf("message = %q, want engine + served model", msg)
		}
	})

	t.Run("docker-less box (runtime unavailable) with nothing serving does not adopt", func(t *testing.T) {
		// Nothing to adopt on a runtime-less box -> fall through so startService
		// fails loudly at its own preflight. The probe MUST run (that is how we
		// learn nothing is serving), but ownership must not be consulted.
		probed, ownershipCalled := false, false
		if _, adopt := maybeAdoptExternalEngineOnStart(
			context.Background(), "vllm", false,
			func() bool { ownershipCalled = true; return false },
			func(context.Context, string, int) ([]string, bool) { probed = true; return nil, false },
		); adopt {
			t.Fatal("expected adopt=false when nothing is serving on a docker-less box")
		}
		if !probed {
			t.Error("expected the port probe to run on a docker-less box (#1084)")
		}
		if ownershipCalled {
			t.Error("must not consult ourContainerRunning when the runtime is unavailable")
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
