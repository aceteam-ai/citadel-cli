package status

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// newTestDiscovery starts an httptest server with the given handler and
// returns a ModelDiscovery pinned to the server's address plus the server's
// port. Pinning host to 127.0.0.1 (httptest's listen address) avoids
// localhost→::1 resolution flakiness.
func newTestDiscovery(t *testing.T, handler http.Handler) (*ModelDiscovery, int) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	addr, ok := server.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener addr type %T", server.Listener.Addr())
	}
	discovery := NewModelDiscovery()
	discovery.host = "127.0.0.1"
	return discovery, addr.Port
}

// openAIModelsHandler serves an OpenAI-compatible GET /v1/models list with the
// given model IDs (the vLLM and llama.cpp dialect).
func openAIModelsHandler(ids ...string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		data := make([]map[string]string, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]string{"id": id, "object": "model"})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	})
	return mux
}

func TestDiscoverModels_VLLM(t *testing.T) {
	discovery, port := newTestDiscovery(t, openAIModelsHandler("Qwen/Qwen3-8B"))

	models, err := discovery.DiscoverModels(context.Background(), "vllm", port)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(models, []string{"Qwen/Qwen3-8B"}) {
		t.Fatalf("expected [Qwen/Qwen3-8B], got %v", models)
	}
}

func TestDiscoverModels_Llamacpp(t *testing.T) {
	discovery, port := newTestDiscovery(t, openAIModelsHandler("/models/llama-3-8b.Q4_K_M.gguf"))

	models, err := discovery.DiscoverModels(context.Background(), "llamacpp", port)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 1 || models[0] != "/models/llama-3-8b.Q4_K_M.gguf" {
		t.Fatalf("expected llamacpp model path, got %v", models)
	}
}

// TestDiscoverModels_LlamacppNoModelLoaded covers llama.cpp being up with NO
// model loaded: a 200 with an empty data list is an empty result, not an
// error.
func TestDiscoverModels_LlamacppNoModelLoaded(t *testing.T) {
	discovery, port := newTestDiscovery(t, openAIModelsHandler())

	models, err := discovery.DiscoverModels(context.Background(), "llamacpp", port)
	if err != nil {
		t.Fatalf("expected no error for empty model list, got: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("expected no models, got %v", models)
	}
}

// TestDiscoverModels_OllamaLoaded verifies ollama discovery uses /api/ps
// (LOADED models), not /api/tags (downloaded models).
func TestDiscoverModels_OllamaLoaded(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"name": "llama3:8b", "model": "llama3:8b", "size": 5137025024},
			},
		})
	})
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		t.Error("discovery must query /api/ps (loaded), not /api/tags (downloaded)")
		http.NotFound(w, r)
	})
	discovery, port := newTestDiscovery(t, mux)

	models, err := discovery.DiscoverModels(context.Background(), "ollama", port)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(models, []string{"llama3:8b"}) {
		t.Fatalf("expected [llama3:8b], got %v", models)
	}
}

// TestDiscoverModels_OllamaNothingLoaded covers a running ollama with no
// models resident in memory: empty result, no error.
func TestDiscoverModels_OllamaNothingLoaded(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"models": []any{}})
	})
	discovery, port := newTestDiscovery(t, mux)

	models, err := discovery.DiscoverModels(context.Background(), "ollama", port)
	if err != nil {
		t.Fatalf("expected no error for empty /api/ps, got: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("expected no models, got %v", models)
	}
}

func TestDiscoverModels_Unreachable(t *testing.T) {
	// Bind then immediately close a listener so the port is known-dead.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	discovery := NewModelDiscovery()
	discovery.host = "127.0.0.1"
	for _, engine := range []string{"vllm", "ollama", "llamacpp"} {
		if _, err := discovery.DiscoverModels(context.Background(), engine, port); err == nil {
			t.Errorf("%s: expected error for unreachable server", engine)
		}
	}
}

// TestDiscoverModels_Timeout verifies a hung engine can't stall discovery
// past the caller's context deadline.
func TestDiscoverModels_Timeout(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	})
	discovery, port := newTestDiscovery(t, mux)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := discovery.DiscoverModels(ctx, "vllm", port)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("discovery did not respect context deadline (took %s)", elapsed)
	}
}

func TestDiscoverModels_HTTPError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "loading", http.StatusServiceUnavailable)
	})
	discovery, port := newTestDiscovery(t, mux)

	if _, err := discovery.DiscoverModels(context.Background(), "vllm", port); err == nil {
		t.Fatal("expected error for non-200 response")
	}
}

func TestDiscoverModelsUnsupportedType(t *testing.T) {
	discovery := NewModelDiscovery()

	_, err := discovery.DiscoverModels(context.Background(), "unsupported", 8000)
	if err == nil {
		t.Fatal("expected error for unsupported service type")
	}
	if !strings.Contains(err.Error(), "unsupported service type") {
		t.Errorf("expected 'unsupported service type' error, got: %v", err)
	}
}

func TestEngineTypeFromName(t *testing.T) {
	cases := map[string]string{
		"vllm":           "vllm",
		"citadel-vllm":   "vllm",
		"ollama":         "ollama", // must NOT match llamacpp despite containing "llama"
		"my-ollama-1":    "ollama",
		"llamacpp":       "llamacpp",
		"llama.cpp":      "llamacpp",
		"llama-cpp":      "llamacpp",
		"bonsai":         "bonsai",
		"citadel-bonsai": "bonsai",
		"transcribe":     "",
		"gotenberg":      "",
		"postgres":       "",
		"LLAMACPP-serve": "llamacpp",
	}
	for name, want := range cases {
		if got := EngineTypeFromName(name); got != want {
			t.Errorf("EngineTypeFromName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestCheckServiceHealth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Ollama is running"))
	})
	discovery, port := newTestDiscovery(t, mux)

	for _, engine := range []string{"vllm", "llamacpp", "ollama"} {
		health, err := discovery.CheckServiceHealth(context.Background(), engine, port)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", engine, err)
		}
		if health != HealthStatusOK {
			t.Errorf("%s: expected ok, got %s", engine, health)
		}
	}
}

func TestCheckServiceHealthUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	discovery := NewModelDiscovery()
	discovery.host = "127.0.0.1"
	for _, engine := range []string{"vllm", "llamacpp", "ollama"} {
		health, err := discovery.CheckServiceHealth(context.Background(), engine, port)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", engine, err)
		}
		if health != HealthStatusUnhealthy {
			t.Errorf("%s: expected unhealthy for unreachable server, got %s", engine, health)
		}
	}
}

func TestCheckServiceHealthUnknownType(t *testing.T) {
	discovery := NewModelDiscovery()

	health, err := discovery.CheckServiceHealth(context.Background(), "unknown", 8000)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if health != HealthStatusUnknown {
		t.Errorf("expected 'unknown' for unsupported type, got '%s'", health)
	}
}

func TestNewModelDiscovery(t *testing.T) {
	discovery := NewModelDiscovery()

	if discovery == nil {
		t.Fatal("expected non-nil discovery")
	}
	if discovery.httpClient == nil {
		t.Error("expected non-nil http client")
	}
	if discovery.host != "localhost" {
		t.Errorf("expected default host localhost, got %q", discovery.host)
	}
}
