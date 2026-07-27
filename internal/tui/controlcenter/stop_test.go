package controlcenter

import "testing"

// TestIsFabricManagedEngine covers the predicate that gates the "fabric may
// restart this" stop warning: inference engines match, plain media/tool services
// do not.
func TestIsFabricManagedEngine(t *testing.T) {
	engines := []string{"vllm", "ollama", "llamacpp", "llama.cpp", "bonsai", "sglang", "lmstudio", "citadel-vllm", "VLLM"}
	for _, n := range engines {
		if !isFabricManagedEngine(n) {
			t.Errorf("isFabricManagedEngine(%q) = false, want true", n)
		}
	}
	nonEngines := []string{"kokoro", "meeting", "gotenberg", "transcribe", "nvr", "frigate", ""}
	for _, n := range nonEngines {
		if isFabricManagedEngine(n) {
			t.Errorf("isFabricManagedEngine(%q) = true, want false", n)
		}
	}
}

// TestStopIdempotent verifies that Stop() can be called multiple times without
// panicking on a double-close of stopChan. Stop() is reachable from three
// concurrent paths during shutdown (the Ctrl+C key handler, the quit-confirm
// modal, and the OS signal handler), so it must be safe to invoke more than
// once (issue #312). With app/consolePage/chatPage nil on a freshly-built
// control center, Stop() only closes stopChan, which is exactly the path the
// sync.Once guard protects.
func TestStopIdempotent(t *testing.T) {
	cc := New(Config{Version: "test"})

	cc.Stop()
	cc.Stop() // second call must not panic (double-close of stopChan)

	select {
	case <-cc.stopChan:
		// closed as expected
	default:
		t.Fatal("stopChan was not closed by Stop()")
	}
}
