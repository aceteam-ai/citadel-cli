package services

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
)

func sp(s string) *string { return &s }

func TestValidateOllamaKeepAlive(t *testing.T) {
	for _, ok := range []string{"-1", "30m", "24h", "90s", "1h30m"} {
		if err := ValidateOllamaKeepAlive(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "0", "-2", "-1s", "30", "abc", "0s", "-5m"} {
		if err := ValidateOllamaKeepAlive(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestOllamaKeepAliveComposeEnv(t *testing.T) {
	base := []string{"A=1", "OLLAMA_KEEP_ALIVE=hostile", "B=2"}
	env, err := OllamaKeepAliveComposeEnv(base, "ollama", sp("24h"))
	if err != nil || env[len(env)-1] != "OLLAMA_KEEP_ALIVE=24h" || slices.Contains(env, "OLLAMA_KEEP_ALIVE=hostile") {
		t.Fatalf("set: %v %v", env, err)
	}
	// Unset: byte-identical.
	env, err = OllamaKeepAliveComposeEnv(base, "ollama", nil)
	if err != nil || !slices.Equal(env, base) {
		t.Fatalf("unset: %v %v", env, err)
	}
	// Invalid: error, env unchanged.
	env, err = OllamaKeepAliveComposeEnv(base, "ollama", sp("bogus"))
	if err == nil || !slices.Equal(env, base) {
		t.Fatalf("invalid: %v %v", env, err)
	}
	// Non-ollama: byte-identical, input not mutated.
	env, err = OllamaKeepAliveComposeEnv(base, "vllm", sp("24h"))
	if err != nil || !slices.Equal(env, base) {
		t.Fatalf("vllm: %v %v", env, err)
	}
}

func TestPreloadOllamaModelsCallsAPIWithKeepAlive(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.WriteHeader(200)
		case "/api/generate":
			raw, _ := io.ReadAll(r.Body)
			var m map[string]any
			json.Unmarshal(raw, &m)
			mu.Lock()
			bodies = append(bodies, m)
			mu.Unlock()
			if m["model"] == "bad" {
				w.WriteHeader(500)
				return
			}
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()

	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, f) }
	PreloadOllamaModels(context.Background(), srv.Client(), srv.URL, []string{"bad", " qwen3.5:4b ", ""}, "-1", logf)
	if len(bodies) != 2 {
		t.Fatalf("want 2 generate calls (failure must not stop the rest), got %d", len(bodies))
	}
	if bodies[1]["model"] != "qwen3.5:4b" || bodies[1]["keep_alive"] != float64(-1) {
		t.Errorf("keep_alive -1 must be a JSON number: %v", bodies[1])
	}
	if len(logs) != 2 {
		t.Errorf("want one failure + one success log, got %v", logs)
	}

	bodies = nil
	PreloadOllamaModels(context.Background(), srv.Client(), srv.URL, []string{"m"}, "30m", logf)
	if bodies[0]["keep_alive"] != "30m" {
		t.Errorf("duration keep_alive: %v", bodies[0])
	}
	bodies = nil
	PreloadOllamaModels(context.Background(), srv.Client(), srv.URL, []string{"m"}, "", logf)
	if _, ok := bodies[0]["keep_alive"]; ok {
		t.Errorf("unset keep_alive must be omitted: %v", bodies[0])
	}
}
