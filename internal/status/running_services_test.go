package status

import (
	"reflect"
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

// stubRunningContainers swaps the container-runtime enumeration for a fixed
// list, so the SELECTION logic (which containers become reported services) is
// testable without docker/podman. Restored on test cleanup.
func stubRunningContainers(t *testing.T, names ...string) {
	t.Helper()
	prev := runningContainerNames
	runningContainerNames = func(string) []string { return names }
	t.Cleanup(func() { runningContainerNames = prev })
}

// TestRunningEmbeddedServices_MapsContainerNames is the core of aceteam#7148:
// the inventory must come from the containers that are actually up, not from a
// hardcoded probe list.
func TestRunningEmbeddedServices_MapsContainerNames(t *testing.T) {
	stubRunningContainers(t,
		"citadel-tei",
		"citadel-kokoro",
		"citadel-claudecode", // a MODULE, not an embedded service
		"some-unrelated-container",
	)

	got := runningEmbeddedServices("docker")

	want := map[string]bool{"tei": true, "kokoro": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runningEmbeddedServices = %v, want %v", got, want)
	}
}

// TestRunningEmbeddedServices_RuntimeFailureIsEmpty guards the degradation
// posture: an unavailable container runtime must yield "nothing enumerable",
// never a panic and never a fabricated entry.
func TestRunningEmbeddedServices_RuntimeFailureIsEmpty(t *testing.T) {
	prev := runningContainerNames
	runningContainerNames = func(string) []string { return nil }
	t.Cleanup(func() { runningContainerNames = prev })

	if got := runningEmbeddedServices("docker"); len(got) != 0 {
		t.Fatalf("expected empty set when the runtime lists nothing, got %v", got)
	}
}

// TestCollectRunningEmbeddedServices_CoversEveryEmbeddedEngine is the
// regression the issue asks for by name: EVERY embedded-compose engine must be
// reportable, not just the six that happen to have a probe. Before #7148,
// kokoro/transcribe/diffusers/extraction/sglang/lmstudio had no path into the
// heartbeat at all.
func TestCollectRunningEmbeddedServices_CoversEveryEmbeddedEngine(t *testing.T) {
	for name := range services.ServiceMap {
		running := map[string]bool{name: true}
		got := collectRunningEmbeddedServices(running, map[string]struct{}{})
		if len(got) != 1 {
			t.Fatalf("embedded service %q running but not reported (got %d entries)", name, len(got))
		}
		if got[0].Name != name {
			t.Fatalf("expected service %q, got %q", name, got[0].Name)
		}
		if got[0].Status != ServiceStatusRunning {
			t.Errorf("service %q: status = %q, want %q", name, got[0].Status, ServiceStatusRunning)
		}
		// No readiness probe ran, so health must be unknown rather than a
		// fabricated "ok".
		if got[0].Health != HealthStatusUnknown {
			t.Errorf("service %q: health = %q, want %q", name, got[0].Health, HealthStatusUnknown)
		}
	}
}

// TestCollectRunningEmbeddedServices_SkipsAlreadyReported keeps the coarse
// backstop from overwriting the richer probe-derived entry (models, idle state)
// for a service a real collector already described.
func TestCollectRunningEmbeddedServices_SkipsAlreadyReported(t *testing.T) {
	running := map[string]bool{"vllm": true, "kokoro": true}
	reported := map[string]struct{}{"vllm": {}}

	got := collectRunningEmbeddedServices(running, reported)

	if len(got) != 1 || got[0].Name != "kokoro" {
		t.Fatalf("expected only kokoro, got %+v", got)
	}
}

// TestCollectRunningEmbeddedServices_Deterministic guards against a heartbeat
// whose service order churns every 30s purely from Go map iteration.
func TestCollectRunningEmbeddedServices_Deterministic(t *testing.T) {
	running := map[string]bool{"sglang": true, "kokoro": true, "diffusers": true}
	want := []string{"diffusers", "kokoro", "sglang"}

	for i := 0; i < 5; i++ {
		got := collectRunningEmbeddedServices(running, map[string]struct{}{})
		names := make([]string, 0, len(got))
		for _, svc := range got {
			names = append(names, svc.Name)
		}
		if !reflect.DeepEqual(names, want) {
			t.Fatalf("iteration %d: got %v, want %v", i, names, want)
		}
	}
}

// TestEmbeddedServiceType_OnlyChatEnginesAreLLM guards the routing blast radius
// of the new backstop pass: ServiceTypeLLM is what offers a service to the
// chat/model surfaces, so a TTS or transcription service must never claim it.
func TestEmbeddedServiceType_OnlyChatEnginesAreLLM(t *testing.T) {
	if got := embeddedServiceType("tei"); got != ServiceTypeEmbedding {
		t.Errorf("tei type = %q, want %q", got, ServiceTypeEmbedding)
	}
	for _, name := range []string{"vllm", "ollama", "llamacpp", "bonsai", "unlimited-ocr"} {
		if got := embeddedServiceType(name); got != ServiceTypeLLM {
			t.Errorf("%s type = %q, want %q", name, got, ServiceTypeLLM)
		}
	}
	for _, name := range []string{"kokoro", "transcribe", "diffusers", "extraction"} {
		if got := embeddedServiceType(name); got != ServiceTypeOther {
			t.Errorf("%s type = %q, want %q (llm would make it a chat-router candidate)",
				name, got, ServiceTypeOther)
		}
	}
}

// TestEnginePortIfRunning_UsesRunningSet confirms the collectors read the
// once-per-heartbeat container set instead of shelling out per engine.
func TestEnginePortIfRunning_UsesRunningSet(t *testing.T) {
	port, running := enginePortIfRunning(map[string]bool{"vllm": true}, "vllm")
	if !running {
		t.Fatal("vllm in the running set must be reported running")
	}
	if port != services.VLLMHostPort {
		t.Fatalf("port = %d, want %d", port, services.VLLMHostPort)
	}
}
