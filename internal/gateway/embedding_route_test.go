package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestEmbeddingUpstreamRouting verifies that a /v1/embeddings request is routed
// to the registered embedding (TEI) upstream and not to an unrelated backend
// (issue #351). The path is forwarded unmodified (no prefix stripping), so TEI's
// OpenAI-compatible /v1/embeddings handler receives it verbatim.
func TestEmbeddingUpstreamRouting(t *testing.T) {
	teiBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"backend": "tei",
			"path":    r.URL.Path,
		})
	}))
	defer teiBackend.Close()

	statusBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"backend": "status"})
	}))
	defer statusBackend.Close()

	teiAddr := teiBackend.URL[len("http://"):]
	statusAddr := statusBackend.URL[len("http://"):]

	gw := NewServer(Config{Port: 0, NodeName: "test-node"})
	gw.AddUpstream("/health", &Upstream{Address: statusAddr})
	gw.AddUpstream("/v1/embeddings", &Upstream{Address: teiAddr})

	for prefix, upstream := range gw.config.Upstreams {
		gw.registerProxy(prefix, upstream)
	}
	gw.mux.HandleFunc("/", gw.handleRoot)

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", nil)
	w := httptest.NewRecorder()
	gw.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response: %v", err)
	}
	if resp["backend"] != "tei" {
		t.Errorf("routed to %q backend, want tei", resp["backend"])
	}
	if resp["path"] != "/v1/embeddings" {
		t.Errorf("proxied path = %q, want /v1/embeddings (no strip)", resp["path"])
	}
}
