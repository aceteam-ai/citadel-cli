package status

import (
	"context"
	"net"
	"net/http"
	"testing"
)

// TestOpenAICompatServing_AdoptsOnlyOpenAIShaped is the aceteam-ai/citadel-cli#1081
// adoption-detection contract: a 200 GET /v1/models with an OpenAI-shaped body is
// the ONLY thing adopted. A non-200 (404), a 200 with a non-OpenAI body, and
// nothing listening at all are all "not serving" -- an arbitrary TCP listener is
// never adopted.
func TestOpenAICompatServing_AdoptsOnlyOpenAIShaped(t *testing.T) {
	t.Run("openai /v1/models 200 -> serving, models surfaced", func(t *testing.T) {
		md, port := newTestDiscovery(t, openAIModelsHandler("Qwen/Qwen3-8B"))
		models, serving := openAICompatServing(context.Background(), md, "vllm", port)
		if !serving {
			t.Fatal("expected serving=true for an OpenAI /v1/models 200")
		}
		if len(models) != 1 || models[0] != "Qwen/Qwen3-8B" {
			t.Fatalf("expected [Qwen/Qwen3-8B], got %v", models)
		}
	})

	t.Run("200 with empty data list -> serving (engine up, no model loaded)", func(t *testing.T) {
		// Consistent with DiscoverLocalEngines: a running engine with no model
		// loaded is real serving state, not a failure.
		md, port := newTestDiscovery(t, openAIModelsHandler())
		models, serving := openAICompatServing(context.Background(), md, "vllm", port)
		if !serving {
			t.Fatal("expected serving=true for a 200 with an empty data list")
		}
		if len(models) != 0 {
			t.Fatalf("expected no models, got %v", models)
		}
	})

	t.Run("404 -> not serving", func(t *testing.T) {
		md, port := newTestDiscovery(t, http.NotFoundHandler())
		if _, serving := openAICompatServing(context.Background(), md, "vllm", port); serving {
			t.Fatal("expected serving=false for a 404 (not OpenAI-compat)")
		}
	})

	t.Run("200 non-OpenAI body -> not serving", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("hello, i am not a models list"))
		})
		md, port := newTestDiscovery(t, mux)
		if _, serving := openAICompatServing(context.Background(), md, "vllm", port); serving {
			t.Fatal("expected serving=false for a 200 with a non-OpenAI body (arbitrary listener)")
		}
	})

	t.Run("nothing listening -> not serving", func(t *testing.T) {
		// Bind then immediately close to obtain a port with high confidence that
		// nothing is listening on it (connection refused, resolved fast).
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		md := NewModelDiscovery()
		md.host = "127.0.0.1"
		if _, serving := openAICompatServing(context.Background(), md, "vllm", port); serving {
			t.Fatal("expected serving=false when nothing is listening")
		}
	})

	t.Run("port <= 0 -> not serving", func(t *testing.T) {
		md := NewModelDiscovery()
		if _, serving := openAICompatServing(context.Background(), md, "vllm", 0); serving {
			t.Fatal("expected serving=false for a non-positive port")
		}
	})
}

// TestAdoptableExternalEnginePort pins the adoption allowlist: vLLM resolves to
// its citadel-registry host port (CITADEL_VLLM_HOST_PORT-aware via
// managedEngineHostPort), and everything else is not adoptable.
func TestAdoptableExternalEnginePort(t *testing.T) {
	port, ok := AdoptableExternalEnginePort("vllm")
	if !ok {
		t.Fatal("vllm must be adoptable")
	}
	if want := managedEngineHostPort("vllm"); port != want {
		t.Fatalf("vllm adoption port = %d, want the citadel-resolved host port %d", port, want)
	}
	for _, name := range []string{"ollama", "llamacpp", "bonsai", "sglang", "diffusers", ""} {
		if _, ok := AdoptableExternalEnginePort(name); ok {
			t.Errorf("%q must not be adoptable (allowlist is vLLM-only)", name)
		}
	}
}
