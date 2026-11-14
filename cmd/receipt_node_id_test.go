package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

func TestServingReceiptNodeIDIsThePublishedName(t *testing.T) {
	for _, name := range []string{"ubuntu-gpu", " spaced ", ""} {
		if got := servingReceiptNodeID(name); got != name {
			t.Fatalf("servingReceiptNodeID(%q) = %q, want the published name unchanged", name, got)
		}
	}
}

type fakeReceiptSigner struct{}

func (fakeReceiptSigner) Sign([]byte) ([]byte, error) { return []byte("synthetic signature"), nil }
func (fakeReceiptSigner) PublicKeyFingerprint() (string, error) {
	return "sha256:" + strings.Repeat("a", 64), nil
}

// TestBuildNodeJobHandlersChatReceiptSignsRoutingName drives the REAL
// buildNodeJobHandlers builder and its returned LLM handler against a
// loopback fake engine, and asserts the emitted signed receipt carries the
// routing name, not the signer fingerprint. Removing the resolver hookup in
// buildNodeJobHandlers makes this fail (the receipt falls back to the
// fingerprint). The resolver replaces the default device-creds lookup, so the
// test never reads the real node config dir.
func TestBuildNodeJobHandlersChatReceiptSignsRoutingName(t *testing.T) {
	t.Setenv("CITADEL_MODEL_HOTSWAP", "0")
	t.Setenv("CITADEL_GROUNDING_GUARDRAIL", "1")
	t.Setenv("CITADEL_SIGN_AEP_RECEIPTS", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"fixture"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"the answer is 42."},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	const routing = "node-regular"
	handlers, _ := buildNodeJobHandlers(nodeJobHandlerOpts{
		ReceiptRoutingNodeID: routing,
		WorkspaceDir:         t.TempDir(),
		ConfigDir:            t.TempDir(),
		ShellDisabled:        true,
		HandlerLog:           func(string, ...any) {},
	})
	var h *worker.LLMInferenceHandler
	for _, handler := range handlers {
		if v, ok := handler.(*worker.LLMInferenceHandler); ok {
			h = v
			break
		}
	}
	if h == nil {
		t.Fatal("buildNodeJobHandlers returned no LLM inference handler")
	}
	h.WithSigner(fakeReceiptSigner{}).WithOnlyBackends(map[string]string{"bonsai": srv.URL}).WithRequestRecorder(func(string) {})
	job := &worker.Job{ID: "wiring-job", Type: worker.JobTypeLLMInference, Payload: map[string]any{
		"backend": "bonsai", "model": "fixture",
		"messages": []map[string]any{{"role": "user", "content": "fixture"}},
	}}
	result, err := h.Execute(context.Background(), job, &worker.NoOpStreamWriter{})
	if err != nil || result == nil {
		t.Fatalf("Execute failed: %v", err)
	}
	receipt, ok := result.Output["aep_receipt"].(map[string]any)
	if !ok {
		t.Fatalf("no signed receipt emitted; status=%s", result.Status)
	}
	if got := receipt["node_id"]; got != routing {
		t.Fatalf("receipt node_id = %v, want routing name %q", got, routing)
	}
}
