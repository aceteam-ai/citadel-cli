package worker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/aep"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// TestLLMInferenceHandler_CanHandle asserts the handler only claims the
// "llm_inference" job type (issue #590).
// TestAEPSigningStoreDir_PureFunctionOfNodeConfigDir pins that
// aepSigningStoreDir is a deterministic, side-effect-free function of its
// input: the same nodeConfigDir always yields the same signing-store
// directory, and it is rooted at "<nodeConfigDir>/identity" -- i.e. it is a
// child of whatever network.GetNodeConfigDir() resolves to, not a
// hardcoded/absolute path of its own.
func TestAEPSigningStoreDir_PureFunctionOfNodeConfigDir(t *testing.T) {
	converged := filepath.Join(string(filepath.Separator), "var", "lib", "citadel-node")
	want := filepath.Join(converged, "identity")

	if got := aepSigningStoreDir(converged); got != want {
		t.Errorf("aepSigningStoreDir(%q) = %q, want %q", converged, got, want)
	}
	// Deterministic across repeated calls.
	if got1, got2 := aepSigningStoreDir(converged), aepSigningStoreDir(converged); got1 != got2 {
		t.Errorf("aepSigningStoreDir is not deterministic: %q vs %q", got1, got2)
	}
}

// TestDefaultAEPSigner_MachineConvergentAcrossInvocationContexts is the
// coordinator-requested regression pin for the invoker-scoping hazard: it
// proves that TWO independently-constructed nodeidentity.Store instances,
// each standing in for a DIFFERENT invocation context (e.g. `citadel init`
// run interactively vs. `citadel work` run under systemd-root) that both
// legitimately resolve network.GetNodeConfigDir() to the SAME converged
// directory (that convergence property is network.GetNodeConfigDir()'s own
// job, and is pinned by internal/network's own tests -- not re-tested here),
// load/create the IDENTICAL signing key rather than two different ones.
//
// Before this fix, the default signer was nodeidentity.Default() -- rooted
// at invoker-scoped platform.ConfigDir() -- so this same test, run against
// that default, would NOT hold: two different invocation contexts (e.g. a
// non-root interactive shell vs. a root/systemd context) resolve DIFFERENT
// platform.ConfigDir() paths and would silently generate two different
// keypairs.
func TestDefaultAEPSigner_MachineConvergentAcrossInvocationContexts(t *testing.T) {
	convergedNodeConfigDir := t.TempDir() // stands in for network.GetNodeConfigDir()'s converged result

	// Context A: e.g. `citadel init`, run interactively.
	storeA := nodeidentity.New(aepSigningStoreDir(convergedNodeConfigDir))
	keyA, err := storeA.GetOrCreateKey()
	if err != nil {
		t.Fatalf("context A GetOrCreateKey: %v", err)
	}

	// Context B: e.g. `citadel work`, run under systemd-root. A DIFFERENT
	// Store instance, but constructed from the SAME converged directory --
	// exactly what defaultAEPSigner() does via network.GetNodeConfigDir()
	// in each process.
	storeB := nodeidentity.New(aepSigningStoreDir(convergedNodeConfigDir))
	keyB, err := storeB.GetOrCreateKey()
	if err != nil {
		t.Fatalf("context B GetOrCreateKey: %v", err)
	}

	if keyA.D.Cmp(keyB.D) != 0 {
		t.Fatalf("two invocation contexts resolving the same converged node config dir loaded DIFFERENT signing keys -- machine-convergence broken")
	}

	fpA, err := storeA.PublicKeyFingerprint()
	if err != nil {
		t.Fatalf("context A PublicKeyFingerprint: %v", err)
	}
	fpB, err := storeB.PublicKeyFingerprint()
	if err != nil {
		t.Fatalf("context B PublicKeyFingerprint: %v", err)
	}
	if fpA != fpB {
		t.Fatalf("fingerprints diverged across invocation contexts: %q vs %q", fpA, fpB)
	}
}

// TestNewLLMInferenceHandler_SignerIsMachineConvergentNotInvokerScoped pins
// the constructor wiring itself: NewLLMInferenceHandler's default signer
// must be rooted at network.GetNodeConfigDir(), NOT at
// nodeidentity.Default()'s invoker-scoped platform.ConfigDir() (which
// cmd/device.go's separate device-mode flow still legitimately uses).
func TestNewLLMInferenceHandler_SignerIsMachineConvergentNotInvokerScoped(t *testing.T) {
	h := NewLLMInferenceHandler()
	store, ok := h.signer.(*nodeidentity.Store)
	if !ok {
		t.Fatalf("h.signer = %T, want *nodeidentity.Store", h.signer)
	}

	want := aepSigningStoreDir(network.GetNodeConfigDir())
	if store.Dir() != want {
		t.Errorf("default signer store dir = %q, want %q (network.GetNodeConfigDir()-rooted)", store.Dir(), want)
	}

	invokerScoped := filepath.Join(platform.ConfigDir(), "identity")
	if store.Dir() == invokerScoped && want != invokerScoped {
		t.Errorf("default signer store dir unexpectedly matches nodeidentity.Default()'s invoker-scoped path %q", invokerScoped)
	}
}

func TestLLMInferenceHandler_CanHandle(t *testing.T) {
	h := NewLLMInferenceHandler()
	if !h.CanHandle(JobTypeLLMInference) {
		t.Errorf("CanHandle(%q) = false, want true", JobTypeLLMInference)
	}
	for _, other := range []string{"SHELL_COMMAND", "embedding", "VLLM_INFERENCE", ""} {
		if h.CanHandle(other) {
			t.Errorf("CanHandle(%q) = true, want false", other)
		}
	}
}

// newChatCompletionsServer returns an httptest server that emits the given SSE
// frames verbatim (each already a full "data: ..." line) at /v1/chat/completions.
// It records the last request path so routing can be asserted.
func newChatCompletionsServer(t *testing.T, frames []string, lastPath *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A serving engine answers its readiness endpoint (citadel-cli#680); the
		// handler now probes before proxying.
		if serveReadinessProbe(w, r) {
			return
		}
		if lastPath != nil {
			*lastPath = r.URL.Path
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			_, _ = w.Write([]byte(f + "\n"))
		}
	}))
}

// TestLLMInferenceHandler_ChatStreaming drives a streaming chat request through
// the bonsai backend (routes to /v1/chat/completions with no readiness poll) and
// asserts the SSE deltas are forwarded as chunks and accumulated into the final
// JobResult.Output. NOTE: the handler intentionally does NOT call WriteEnd — the
// Runner does that with result.Output (runner.go), so the streaming test asserts
// the WriteChunk captures plus the returned Output.content, not a WriteEnd call.
func TestLLMInferenceHandler_ChatStreaming(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
		`data: {"choices":[{"delta":{"content":", world"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	var gotPath string
	ts := newChatCompletionsServer(t, frames, &gotPath)
	defer ts.Close()

	h := NewLLMInferenceHandler()
	h.baseURLs["bonsai"] = ts.URL

	job := &Job{
		ID:   "job-1",
		Type: JobTypeLLMInference,
		Payload: map[string]any{
			"model":   "bonsai-27b",
			"backend": "bonsai",
			"stream":  true,
			"messages": []map[string]any{
				{"role": "user", "content": "hi"},
			},
		},
	}
	stream := &MockStreamWriter{}
	result, err := h.Execute(context.Background(), job, stream)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result == nil || result.Status != JobStatusSuccess {
		t.Fatalf("Execute result = %+v, want success", result)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("request path = %q, want /v1/chat/completions", gotPath)
	}
	wantChunks := []string{"Hello", ", world"}
	if strings.Join(stream.chunks, "|") != strings.Join(wantChunks, "|") {
		t.Errorf("chunks = %v, want %v", stream.chunks, wantChunks)
	}
	if got, _ := result.Output["content"].(string); got != "Hello, world" {
		t.Errorf("Output content = %q, want %q", got, "Hello, world")
	}
	if got, _ := result.Output["finish_reason"].(string); got != "stop" {
		t.Errorf("Output finish_reason = %q, want %q", got, "stop")
	}
}

// TestLLMInferenceHandler_ChatStreamingReasoningFallback covers the thinking-model
// path: when a stream carries only delta.reasoning_content and no answer (token
// budget spent mid-reasoning, e.g. Bonsai), the reasoning is surfaced as the final
// content so the reply is never blank — mirroring the buffered fallback.
func TestLLMInferenceHandler_ChatStreamingReasoningFallback(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"reasoning_content":"thinking hard..."}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
		`data: [DONE]`,
	}
	ts := newChatCompletionsServer(t, frames, nil)
	defer ts.Close()

	h := NewLLMInferenceHandler()
	h.baseURLs["bonsai"] = ts.URL

	job := &Job{
		ID:   "job-reason",
		Type: JobTypeLLMInference,
		Payload: map[string]any{
			"model":    "bonsai-27b",
			"backend":  "bonsai",
			"stream":   true,
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		},
	}
	stream := &MockStreamWriter{}
	result, err := h.Execute(context.Background(), job, stream)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result == nil || result.Status != JobStatusSuccess {
		t.Fatalf("result = %+v, want success", result)
	}
	if got, _ := result.Output["content"].(string); got != "thinking hard..." {
		t.Errorf("Output content = %q, want reasoning fallback", got)
	}
	// The reasoning is surfaced as a single chunk once no answer was produced.
	if len(stream.chunks) != 1 || stream.chunks[0] != "thinking hard..." {
		t.Errorf("chunks = %v, want [thinking hard...]", stream.chunks)
	}
}

// TestLLMInferenceHandler_BackendRouting asserts that both the bonsai and
// llamacpp backends resolve to the /v1/chat/completions path when the job
// carries messages, and that an unknown backend fails.
func TestLLMInferenceHandler_BackendRouting(t *testing.T) {
	// A non-streamed OpenAI chat-completions body (single buffered response).
	body := `{"choices":[{"message":{"content":"routed-ok"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`

	for _, backend := range []string{"bonsai", "llamacpp"} {
		t.Run(backend, func(t *testing.T) {
			var gotPath string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveReadinessProbe(w, r) {
					return
				}
				gotPath = r.URL.Path
				if r.URL.Path != "/v1/chat/completions" {
					http.Error(w, "unexpected path", http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte(body))
			}))
			defer ts.Close()

			h := NewLLMInferenceHandler()
			h.baseURLs[backend] = ts.URL

			job := &Job{
				ID:   "job-" + backend,
				Type: JobTypeLLMInference,
				Payload: map[string]any{
					"model":   "m",
					"backend": backend,
					"messages": []map[string]any{
						{"role": "user", "content": "hi"},
					},
				},
			}
			stream := &MockStreamWriter{}
			result, err := h.Execute(context.Background(), job, stream)
			if err != nil {
				t.Fatalf("Execute error: %v", err)
			}
			if result == nil || result.Status != JobStatusSuccess {
				t.Fatalf("result = %+v, want success", result)
			}
			if gotPath != "/v1/chat/completions" {
				t.Errorf("%s routed to %q, want /v1/chat/completions", backend, gotPath)
			}
			if got, _ := result.Output["content"].(string); got != "routed-ok" {
				t.Errorf("content = %q, want routed-ok", got)
			}
			// The buffered path emits a single parity chunk before the end.
			if len(stream.chunks) != 1 || stream.chunks[0] != "routed-ok" {
				t.Errorf("chunks = %v, want [routed-ok]", stream.chunks)
			}
		})
	}

	t.Run("ollama messages route to /api/chat", func(t *testing.T) {
		// Regression for issue #6641: a chat request (messages, no prompt) to the
		// ollama backend previously hit /api/generate with an empty prompt, so
		// Ollama loaded the model and returned an empty response -> "No response
		// content" on the platform. It must route to /api/chat and return
		// message.content instead.
		body := `{"model":"llama3.2:1b","message":{"role":"assistant","content":"OK"},` +
			`"done":true,"prompt_eval_count":15,"eval_count":3}`
		var gotPath, gotPrompt string
		var sawMessages bool
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if serveReadinessProbe(w, r) {
				return
			}
			gotPath = r.URL.Path
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			if _, ok := req["messages"]; ok {
				sawMessages = true
			}
			if p, ok := req["prompt"].(string); ok {
				gotPrompt = p
			}
			if r.URL.Path != "/api/chat" {
				http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(body))
		}))
		defer ts.Close()

		h := NewLLMInferenceHandler()
		h.baseURLs["ollama"] = ts.URL

		job := &Job{
			ID:   "job-ollama-chat",
			Type: JobTypeLLMInference,
			Payload: map[string]any{
				"model":    "llama3.2:1b",
				"backend":  "ollama",
				"messages": []map[string]any{{"role": "user", "content": "Reply with exactly: OK"}},
			},
		}
		stream := &MockStreamWriter{}
		result, err := h.Execute(context.Background(), job, stream)
		if err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		if result == nil || result.Status != JobStatusSuccess {
			t.Fatalf("result = %+v, want success", result)
		}
		if gotPath != "/api/chat" {
			t.Errorf("routed to %q, want /api/chat", gotPath)
		}
		if !sawMessages {
			t.Errorf("request did not carry messages")
		}
		if gotPrompt != "" {
			t.Errorf("request carried a prompt %q, want none", gotPrompt)
		}
		if got, _ := result.Output["content"].(string); got != "OK" {
			t.Errorf("content = %q, want OK", got)
		}
		if len(stream.chunks) != 1 || stream.chunks[0] != "OK" {
			t.Errorf("chunks = %v, want [OK]", stream.chunks)
		}
		usage, _ := result.Output["usage"].(map[string]any)
		if usage == nil {
			t.Fatalf("usage missing, want token counts")
		}
		if usage["prompt_tokens"] != 15 || usage["completion_tokens"] != 3 || usage["total_tokens"] != 18 {
			t.Errorf("usage = %v, want prompt=15 completion=3 total=18", usage)
		}
	})

	t.Run("ollama streaming chat routes to /api/chat", func(t *testing.T) {
		frames := []string{
			`{"message":{"role":"assistant","content":"Hel"},"done":false}`,
			`{"message":{"role":"assistant","content":"lo"},"done":false}`,
			`{"message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":9,"eval_count":2}`,
		}
		var gotPath string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if serveReadinessProbe(w, r) {
				return
			}
			gotPath = r.URL.Path
			if r.URL.Path != "/api/chat" {
				http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
				return
			}
			for _, f := range frames {
				_, _ = w.Write([]byte(f + "\n"))
			}
		}))
		defer ts.Close()

		h := NewLLMInferenceHandler()
		h.baseURLs["ollama"] = ts.URL

		job := &Job{
			ID:   "job-ollama-chat-stream",
			Type: JobTypeLLMInference,
			Payload: map[string]any{
				"model":    "llama3.2:1b",
				"backend":  "ollama",
				"stream":   true,
				"messages": []map[string]any{{"role": "user", "content": "hi"}},
			},
		}
		stream := &MockStreamWriter{}
		result, err := h.Execute(context.Background(), job, stream)
		if err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		if result == nil || result.Status != JobStatusSuccess {
			t.Fatalf("result = %+v, want success", result)
		}
		if gotPath != "/api/chat" {
			t.Errorf("routed to %q, want /api/chat", gotPath)
		}
		if strings.Join(stream.chunks, "|") != "Hel|lo" {
			t.Errorf("chunks = %v, want [Hel lo]", stream.chunks)
		}
		if got, _ := result.Output["content"].(string); got != "Hello" {
			t.Errorf("content = %q, want Hello", got)
		}
		usage, _ := result.Output["usage"].(map[string]any)
		if usage == nil || usage["prompt_tokens"] != 9 || usage["completion_tokens"] != 2 {
			t.Errorf("usage = %v, want prompt=9 completion=2", usage)
		}
	})

	t.Run("ollama prompt still routes to /api/generate", func(t *testing.T) {
		var gotPath string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if serveReadinessProbe(w, r) {
				return
			}
			gotPath = r.URL.Path
			if r.URL.Path != "/api/generate" {
				http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"response":"generated"}`))
		}))
		defer ts.Close()

		h := NewLLMInferenceHandler()
		h.baseURLs["ollama"] = ts.URL

		job := &Job{
			ID:   "job-ollama-generate",
			Type: JobTypeLLMInference,
			Payload: map[string]any{
				"model":   "llama3.2:1b",
				"backend": "ollama",
				"prompt":  "hi",
			},
		}
		result, err := h.Execute(context.Background(), job, &MockStreamWriter{})
		if err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		if result == nil || result.Status != JobStatusSuccess {
			t.Fatalf("result = %+v, want success", result)
		}
		if gotPath != "/api/generate" {
			t.Errorf("routed to %q, want /api/generate", gotPath)
		}
		if got, _ := result.Output["content"].(string); got != "generated" {
			t.Errorf("content = %q, want generated", got)
		}
	})

	t.Run("unknown backend fails", func(t *testing.T) {
		h := NewLLMInferenceHandler()
		job := &Job{
			ID:   "job-unknown",
			Type: JobTypeLLMInference,
			Payload: map[string]any{
				"model":   "m",
				"prompt":  "hi",
				"backend": "does-not-exist",
			},
		}
		result, err := h.Execute(context.Background(), job, &MockStreamWriter{})
		if err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		if result == nil || result.Status != JobStatusFailure {
			t.Fatalf("result = %+v, want failure for unknown backend", result)
		}
		if !strings.Contains(result.Error.Error(), "unsupported backend") {
			t.Errorf("error = %v, want 'unsupported backend'", result.Error)
		}
	})
}

// TestPromptTextFromPayload_NilSafe pins that the grounding guardrail's input
// extraction never panics on a nil or empty payload — a defensive edge case
// with no HTTP round-trip needed, since it's a pure function of the payload.
func TestPromptTextFromPayload_NilSafe(t *testing.T) {
	if got := promptTextFromPayload(nil); got != "" {
		t.Errorf("nil payload: got %q, want empty string", got)
	}
	empty := &jobs.LLMInferencePayload{}
	if got := promptTextFromPayload(empty); got != "" {
		t.Errorf("empty payload: got %q, want empty string", got)
	}
	withPrompt := &jobs.LLMInferencePayload{Prompt: "hello"}
	if got := promptTextFromPayload(withPrompt); got != "hello" {
		t.Errorf("prompt payload: got %q, want %q", got, "hello")
	}
}

// TestLLMInferenceHandler_GroundingGuardrailGate pins the opt-in contract for
// the on-node grounding guardrail (citadel #8253, guardrail half) at its one
// wired integration point, bufferedChatCompletions:
//   - CITADEL_GROUNDING_GUARDRAIL unset (default): the output map has NO
//     "grounding" key at all — byte-identical to before the guardrail existed.
//   - CITADEL_GROUNDING_GUARDRAIL=1: the output carries a "grounding" receipt
//     shaped {grounded, score, claims_checked, flagged}, and a genuinely
//     fabricated statistic (a number in the reply absent from the request) is
//     flagged.
func TestLLMInferenceHandler_GroundingGuardrailGate(t *testing.T) {
	// The reply fabricates "68%" — nothing numeric appears in the request
	// messages, mirroring the motivating incident (internal/trust's
	// TestCheckGrounding_FabricatedPercentages_Flagged pins the extraction
	// logic itself; this test only pins that the worker wires it in/out
	// correctly under the env gate).
	body := `{"choices":[{"message":{"content":"68% of respondents agreed."},"finish_reason":"stop"}]}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveReadinessProbe(w, r) {
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	newJob := func() *Job {
		return &Job{
			ID:   "job-grounding",
			Type: JobTypeLLMInference,
			Payload: map[string]any{
				"model":   "m",
				"backend": "bonsai",
				"messages": []map[string]any{
					{"role": "user", "content": "Summarize: a majority of respondents agreed."},
				},
			},
		}
	}

	t.Run("disabled by default: no grounding key", func(t *testing.T) {
		h := NewLLMInferenceHandler()
		h.baseURLs["bonsai"] = ts.URL
		result, err := h.Execute(context.Background(), newJob(), &MockStreamWriter{})
		if err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		if result == nil || result.Status != JobStatusSuccess {
			t.Fatalf("result = %+v, want success", result)
		}
		if _, present := result.Output["grounding"]; present {
			t.Errorf("Output = %+v, want no \"grounding\" key when disabled", result.Output)
		}
	})

	t.Run("enabled: flags the fabricated percentage", func(t *testing.T) {
		t.Setenv(groundingGuardrailEnvVar, "1")
		h := NewLLMInferenceHandler()
		h.baseURLs["bonsai"] = ts.URL
		result, err := h.Execute(context.Background(), newJob(), &MockStreamWriter{})
		if err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		if result == nil || result.Status != JobStatusSuccess {
			t.Fatalf("result = %+v, want success", result)
		}
		grounding, ok := result.Output["grounding"].(map[string]any)
		if !ok {
			t.Fatalf("Output[\"grounding\"] = %#v (%T), want map[string]any", result.Output["grounding"], result.Output["grounding"])
		}
		if grounded, _ := grounding["grounded"].(bool); grounded {
			t.Errorf("grounding[\"grounded\"] = %v, want false (68%% is fabricated)", grounded)
		}
		if checked, _ := grounding["claims_checked"].(int); checked != 1 {
			t.Errorf("grounding[\"claims_checked\"] = %v, want 1", grounding["claims_checked"])
		}
		flagged, ok := grounding["flagged"].([]map[string]any)
		if !ok || len(flagged) != 1 {
			t.Fatalf("grounding[\"flagged\"] = %#v, want one flagged claim", grounding["flagged"])
		}
		if flagged[0]["value"] != "68%" {
			t.Errorf("flagged claim value = %v, want \"68%%\"", flagged[0]["value"])
		}
	})
}

// fakeAEPSigner is an in-memory ECDSA signer implementing aep.Signer, used to
// keep AEP receipt-signing tests hermetic -- unlike nodeidentity.Default()
// (the production default), it never touches the real host's
// platform.ConfigDir()/identity directory.
type fakeAEPSigner struct {
	key *ecdsa.PrivateKey
}

func newFakeAEPSigner(t *testing.T) *fakeAEPSigner {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return &fakeAEPSigner{key: key}
}

func (f *fakeAEPSigner) Sign(payload []byte) ([]byte, error) {
	digest := sha256.Sum256(payload)
	return ecdsa.SignASN1(rand.Reader, f.key, digest[:])
}

func (f *fakeAEPSigner) PublicKeyFingerprint() (string, error) {
	der, err := x509.MarshalPKIXPublicKey(&f.key.PublicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// TestLLMInferenceHandler_SignAEPReceiptsGate pins the opt-in contract for
// the signed AEP receipt (aceteam #8253, the signing half deferred at
// citadel#847's merge -- docs/design-node-identity-receipts.md):
//   - CITADEL_GROUNDING_GUARDRAIL off (regardless of CITADEL_SIGN_AEP_RECEIPTS):
//     byte-identical output to before this feature existed -- no "grounding",
//     no "aep_receipt".
//   - CITADEL_GROUNDING_GUARDRAIL on, CITADEL_SIGN_AEP_RECEIPTS off (the
//     citadel#847 shipped default): "grounding" attaches, "aep_receipt" does
//     NOT -- signing is a second, independent opt-in.
//   - Both on: "aep_receipt" attaches, shaped as designed, and its signature
//     verifies against the injected signer's own public key.
func TestLLMInferenceHandler_SignAEPReceiptsGate(t *testing.T) {
	body := `{"choices":[{"message":{"content":"the answer is 42."},"finish_reason":"stop"}]}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveReadinessProbe(w, r) {
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	newJob := func() *Job {
		return &Job{
			ID:   "job-aep-1",
			Type: JobTypeLLMInference,
			Payload: map[string]any{
				"model":   "bonsai-27b",
				"backend": "bonsai",
				"messages": []map[string]any{
					{"role": "user", "content": "what is the answer?"},
				},
			},
		}
	}

	t.Run("both off: no grounding, no aep_receipt", func(t *testing.T) {
		h := NewLLMInferenceHandler().WithSigner(newFakeAEPSigner(t)).WithFabricNodeIDResolver(func() string { return "" })
		h.baseURLs["bonsai"] = ts.URL
		result, err := h.Execute(context.Background(), newJob(), &MockStreamWriter{})
		if err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		if _, present := result.Output["grounding"]; present {
			t.Errorf("Output = %+v, want no grounding key", result.Output)
		}
		if _, present := result.Output["aep_receipt"]; present {
			t.Errorf("Output = %+v, want no aep_receipt key", result.Output)
		}
	})

	t.Run("grounding on, signing off: grounding attaches, aep_receipt does not", func(t *testing.T) {
		t.Setenv(groundingGuardrailEnvVar, "1")
		h := NewLLMInferenceHandler().WithSigner(newFakeAEPSigner(t)).WithFabricNodeIDResolver(func() string { return "" })
		h.baseURLs["bonsai"] = ts.URL
		result, err := h.Execute(context.Background(), newJob(), &MockStreamWriter{})
		if err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		if _, present := result.Output["grounding"]; !present {
			t.Errorf("Output = %+v, want a grounding key", result.Output)
		}
		if _, present := result.Output["aep_receipt"]; present {
			t.Errorf("Output = %+v, want no aep_receipt key when signing is off", result.Output)
		}
	})

	t.Run("signing on but grounding off: aep_receipt does not attach either", func(t *testing.T) {
		// Signing rides INSIDE the grounding gate (it signs the
		// GroundingResult) -- CITADEL_SIGN_AEP_RECEIPTS alone, without the
		// guardrail itself on, must be a no-op.
		t.Setenv(signAEPReceiptsEnvVar, "1")
		h := NewLLMInferenceHandler().WithSigner(newFakeAEPSigner(t)).WithFabricNodeIDResolver(func() string { return "" })
		h.baseURLs["bonsai"] = ts.URL
		result, err := h.Execute(context.Background(), newJob(), &MockStreamWriter{})
		if err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		if _, present := result.Output["grounding"]; present {
			t.Errorf("Output = %+v, want no grounding key", result.Output)
		}
		if _, present := result.Output["aep_receipt"]; present {
			t.Errorf("Output = %+v, want no aep_receipt key when grounding is off", result.Output)
		}
	})

	t.Run("both on: aep_receipt attaches and verifies", func(t *testing.T) {
		t.Setenv(groundingGuardrailEnvVar, "1")
		t.Setenv(signAEPReceiptsEnvVar, "1")
		signer := newFakeAEPSigner(t)
		h := NewLLMInferenceHandler().WithSigner(signer).WithFabricNodeIDResolver(func() string { return "" })
		h.baseURLs["bonsai"] = ts.URL

		job := newJob()
		result, err := h.Execute(context.Background(), job, &MockStreamWriter{})
		if err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		if _, present := result.Output["grounding"]; !present {
			t.Fatalf("Output = %+v, want a grounding key", result.Output)
		}

		raw, present := result.Output["aep_receipt"]
		if !present {
			t.Fatalf("Output = %+v, want an aep_receipt key", result.Output)
		}
		// MUST be a plain map[string]any, not *aep.AEPReceiptV1 -- Output
		// crosses the wire via StreamWriter/Redis/API serialization
		// elsewhere in the worker, so a typed Go pointer attached here would
		// be the only one of its kind in this map. Proving json.Marshal
		// produces the expected wire shape (not just that the in-process Go
		// value looks right) is the point of this assertion.
		receiptMap, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("aep_receipt = %#v (%T), want map[string]any", raw, raw)
		}
		wireBytes, err := json.Marshal(result.Output)
		if err != nil {
			t.Fatalf("json.Marshal(result.Output): %v", err)
		}
		var wire struct {
			AEPReceipt aep.AEPReceiptV1 `json:"aep_receipt"`
		}
		if err := json.Unmarshal(wireBytes, &wire); err != nil {
			t.Fatalf("json.Unmarshal wire bytes: %v", err)
		}
		receipt := &wire.AEPReceipt

		if receipt.JobID != job.ID {
			t.Errorf("receipt.JobID = %q, want %q", receipt.JobID, job.ID)
		}
		if receipt.Engine != "bonsai" {
			t.Errorf("receipt.Engine = %q, want bonsai", receipt.Engine)
		}
		if receipt.Model != "bonsai-27b" {
			t.Errorf("receipt.Model = %q, want bonsai-27b", receipt.Model)
		}
		wantFP, _ := signer.PublicKeyFingerprint()
		if receipt.NodeID != wantFP {
			t.Errorf("receipt.NodeID = %q, want the signer's own fingerprint %q (no fabric node ID configured in this test)", receipt.NodeID, wantFP)
		}
		if receipt.PublicKeyFingerprint != wantFP {
			t.Errorf("receipt.PublicKeyFingerprint = %q, want %q", receipt.PublicKeyFingerprint, wantFP)
		}
		if receiptMap["job_id"] != job.ID {
			t.Errorf(`receiptMap["job_id"] = %v, want %q (snake_case wire key)`, receiptMap["job_id"], job.ID)
		}

		sigDER, err := base64.StdEncoding.DecodeString(receipt.Signature)
		if err != nil {
			t.Fatalf("decode signature: %v", err)
		}
		digest := sha256.Sum256(aep.Canonicalize(receipt))
		if !ecdsa.VerifyASN1(&signer.key.PublicKey, digest[:], sigDER) {
			t.Errorf("receipt signature does not verify against the signer's own public key")
		}
	})
}

// failingAEPSigner always errors, used to pin the fail-open contract: a
// signing failure must never fail the job or drop the "content"/"grounding"
// fields already computed -- only the aep_receipt attachment is skipped.
type failingAEPSigner struct{}

func (failingAEPSigner) Sign([]byte) ([]byte, error) {
	return nil, errFakeSignerBoom
}

func (failingAEPSigner) PublicKeyFingerprint() (string, error) {
	return "", errFakeSignerBoom
}

var errFakeSignerBoom = fmt.Errorf("fake signer: boom")

// TestLLMInferenceHandler_SignAEPReceiptFailsOpen pins that a signing failure
// (e.g. the node's key is unavailable) never fails the job or drops content
// already computed -- only the aep_receipt attachment is skipped. This is
// the "signing must never break inference" contract stated in
// bufferedChatCompletions' doc comment.
func TestLLMInferenceHandler_SignAEPReceiptFailsOpen(t *testing.T) {
	body := `{"choices":[{"message":{"content":"the answer is 42."},"finish_reason":"stop"}]}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveReadinessProbe(w, r) {
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	t.Setenv(groundingGuardrailEnvVar, "1")
	t.Setenv(signAEPReceiptsEnvVar, "1")

	h := NewLLMInferenceHandler().
		WithSigner(failingAEPSigner{}).
		WithFabricNodeIDResolver(func() string { return "" })
	h.baseURLs["bonsai"] = ts.URL

	var loggedCalls int
	h.aepLogf = func(format string, args ...any) { loggedCalls++ }

	job := &Job{
		ID:   "job-fail-open",
		Type: JobTypeLLMInference,
		Payload: map[string]any{
			"model":    "bonsai-27b",
			"backend":  "bonsai",
			"messages": []map[string]any{{"role": "user", "content": "what is the answer?"}},
		},
	}
	result, err := h.Execute(context.Background(), job, &MockStreamWriter{})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result == nil || result.Status != JobStatusSuccess {
		t.Fatalf("result = %+v, want success even when signing fails", result)
	}
	if got, _ := result.Output["content"].(string); got != "the answer is 42." {
		t.Errorf("content = %q, want the answer to still be present", got)
	}
	if _, present := result.Output["grounding"]; !present {
		t.Errorf("Output = %+v, want the grounding key to still attach", result.Output)
	}
	if _, present := result.Output["aep_receipt"]; present {
		t.Errorf("Output = %+v, want no aep_receipt key when signing fails", result.Output)
	}
	if loggedCalls != 1 {
		t.Errorf("aepLogf called %d times, want exactly 1 (the signing failure logged, non-fatally)", loggedCalls)
	}
}

// --- Tool calling (citadel-cli#603, aceteam #6555) ---------------------------

// TestLLMInferenceHandler_ToolsRequestByteIdenticalWithoutTools pins the
// "no tools key ⇒ identical behavior" contract from #603's definition of done:
// a job payload with no tools/tool_choice and plain text messages must not
// gain ANY new key on the outbound engine request -- not even an empty one.
// Asserting key ABSENCE (not just an empty value) is deliberate: it is the
// assertion that fails if a future edit turns `if len(payload.Tools) > 0`
// into an unconditional assignment.
func TestLLMInferenceHandler_ToolsRequestByteIdenticalWithoutTools(t *testing.T) {
	var gotReq map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveReadinessProbe(w, r) {
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer ts.Close()

	h := NewLLMInferenceHandler()
	h.baseURLs["bonsai"] = ts.URL

	job := &Job{
		ID:   "job-no-tools",
		Type: JobTypeLLMInference,
		Payload: map[string]any{
			"model":    "m",
			"backend":  "bonsai",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		},
	}
	result, err := h.Execute(context.Background(), job, &MockStreamWriter{})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result == nil || result.Status != JobStatusSuccess {
		t.Fatalf("result = %+v, want success", result)
	}

	if _, present := gotReq["tools"]; present {
		t.Errorf("outbound request = %+v, want no \"tools\" key", gotReq)
	}
	if _, present := gotReq["tool_choice"]; present {
		t.Errorf("outbound request = %+v, want no \"tool_choice\" key", gotReq)
	}
	msgs, _ := gotReq["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("outbound messages = %v, want exactly one", msgs)
	}
	msg, _ := msgs[0].(map[string]any)
	for _, key := range []string{"tool_calls", "tool_call_id", "name"} {
		if _, present := msg[key]; present {
			t.Errorf("outbound message = %+v, want no %q key", msg, key)
		}
	}
	if _, present := result.Output["tool_calls"]; present {
		t.Errorf("Output = %+v, want no \"tool_calls\" key", result.Output)
	}
}

// TestLLMInferenceHandler_ToolsForwardedInRequest asserts that tools,
// tool_choice, and a message history carrying assistant tool_calls plus a
// tool-role result (tool_call_id/name) all reach the engine's outbound
// request intact -- the request-build half of #603.
func TestLLMInferenceHandler_ToolsForwardedInRequest(t *testing.T) {
	var gotReq map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveReadinessProbe(w, r) {
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"the weather is sunny"},"finish_reason":"stop"}]}`))
	}))
	defer ts.Close()

	h := NewLLMInferenceHandler()
	h.baseURLs["vllm"] = ts.URL

	job := &Job{
		ID:   "job-tools-request",
		Type: JobTypeLLMInference,
		Payload: map[string]any{
			"model":   "m",
			"backend": "vllm",
			"tools": []map[string]any{
				{
					"type": "function",
					"function": map[string]any{
						"name":        "get_weather",
						"description": "Get the weather for a city",
						"parameters": map[string]any{
							"type":       "object",
							"properties": map[string]any{"city": map[string]any{"type": "string"}},
						},
					},
				},
			},
			"tool_choice": "auto",
			"messages": []map[string]any{
				{"role": "user", "content": "weather in SF?"},
				{
					"role":    "assistant",
					"content": "",
					"tool_calls": []map[string]any{
						{
							"id":   "c1",
							"type": "function",
							"function": map[string]any{
								"name":      "get_weather",
								"arguments": `{"city": "SF"}`,
							},
						},
					},
				},
				{
					"role":         "tool",
					"content":      `{"temp": 60}`,
					"tool_call_id": "c1",
					"name":         "get_weather",
				},
			},
		},
	}
	result, err := h.Execute(context.Background(), job, &MockStreamWriter{})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result == nil || result.Status != JobStatusSuccess {
		t.Fatalf("result = %+v, want success", result)
	}

	tools, ok := gotReq["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("outbound tools = %#v, want a single-element array", gotReq["tools"])
	}
	toolFn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if toolFn["name"] != "get_weather" {
		t.Errorf("outbound tool function name = %v, want get_weather", toolFn["name"])
	}
	if gotReq["tool_choice"] != "auto" {
		t.Errorf("outbound tool_choice = %v, want \"auto\"", gotReq["tool_choice"])
	}

	msgs, _ := gotReq["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("outbound messages = %v, want 3", msgs)
	}
	assistantMsg, _ := msgs[1].(map[string]any)
	assistantToolCalls, ok := assistantMsg["tool_calls"].([]any)
	if !ok || len(assistantToolCalls) != 1 {
		t.Fatalf("assistant message tool_calls = %#v, want single-element array", assistantMsg["tool_calls"])
	}
	tc0, _ := assistantToolCalls[0].(map[string]any)
	if tc0["id"] != "c1" {
		t.Errorf("assistant tool_calls[0].id = %v, want c1", tc0["id"])
	}

	toolResultMsg, _ := msgs[2].(map[string]any)
	if toolResultMsg["tool_call_id"] != "c1" {
		t.Errorf("tool-role message tool_call_id = %v, want c1", toolResultMsg["tool_call_id"])
	}
	if toolResultMsg["name"] != "get_weather" {
		t.Errorf("tool-role message name = %v, want get_weather", toolResultMsg["name"])
	}
}

// TestLLMInferenceHandler_BufferedToolCallsResponse asserts that a buffered
// (non-streaming) engine response carrying message.tool_calls surfaces them
// on JobResult.Output, with finish_reason forwarded as "tool_calls" and no
// spurious empty-content chunk published (a tool_calls-only reply has no text
// worth publishing as a chunk).
func TestLLMInferenceHandler_BufferedToolCallsResponse(t *testing.T) {
	body := `{"choices":[{"message":{"content":"","tool_calls":[` +
		`{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}` +
		`]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveReadinessProbe(w, r) {
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	h := NewLLMInferenceHandler()
	h.baseURLs["bonsai"] = ts.URL

	job := &Job{
		ID:   "job-buffered-tool-calls",
		Type: JobTypeLLMInference,
		Payload: map[string]any{
			"model":   "m",
			"backend": "bonsai",
			"tools": []map[string]any{
				{"type": "function", "function": map[string]any{"name": "get_weather"}},
			},
			"messages": []map[string]any{{"role": "user", "content": "weather in SF?"}},
		},
	}
	stream := &MockStreamWriter{}
	result, err := h.Execute(context.Background(), job, stream)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result == nil || result.Status != JobStatusSuccess {
		t.Fatalf("result = %+v, want success", result)
	}
	if got, _ := result.Output["finish_reason"].(string); got != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", got)
	}
	if got, _ := result.Output["content"].(string); got != "" {
		t.Errorf("content = %q, want empty (tool_calls-only reply)", got)
	}
	raw, ok := result.Output["tool_calls"].(json.RawMessage)
	if !ok {
		t.Fatalf("Output[\"tool_calls\"] = %#v (%T), want json.RawMessage", result.Output["tool_calls"], result.Output["tool_calls"])
	}
	var parsed []map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("tool_calls did not unmarshal as an array: %v", err)
	}
	if len(parsed) != 1 || parsed[0]["id"] != "call_1" {
		t.Errorf("tool_calls = %v, want one entry with id call_1", parsed)
	}
	fn, _ := parsed[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("tool_calls[0].function.name = %v, want get_weather", fn["name"])
	}
	if len(stream.chunks) != 0 {
		t.Errorf("chunks = %v, want none published for a tool_calls-only reply", stream.chunks)
	}
}

// TestLLMInferenceHandler_StreamingToolCallsAccumulate drives a tool-calling
// SSE stream where the tool_calls delta arrives split across multiple frames
// (id+name on the first, arguments fragments on the next two -- the real
// OpenAI streaming shape) and asserts the merged, final tool_calls array is
// returned on JobResult.Output with finish_reason "tool_calls". Per this
// file's streamChatCompletions doc comment, no per-chunk tool_calls delta is
// asserted here -- only the terminal result, which is what the real consumer
// (aceteam's FabricInferenceClient._event_to_chunk "end" branch) reads.
func TestLLMInferenceHandler_StreamingToolCallsAccumulate(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"SF\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}
	ts := newChatCompletionsServer(t, frames, nil)
	defer ts.Close()

	h := NewLLMInferenceHandler()
	h.baseURLs["bonsai"] = ts.URL

	job := &Job{
		ID:   "job-streaming-tool-calls",
		Type: JobTypeLLMInference,
		Payload: map[string]any{
			"model":   "bonsai-27b",
			"backend": "bonsai",
			"stream":  true,
			"tools": []map[string]any{
				{"type": "function", "function": map[string]any{"name": "get_weather"}},
			},
			"messages": []map[string]any{{"role": "user", "content": "weather in SF?"}},
		},
	}
	stream := &MockStreamWriter{}
	result, err := h.Execute(context.Background(), job, stream)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result == nil || result.Status != JobStatusSuccess {
		t.Fatalf("result = %+v, want success", result)
	}
	if got, _ := result.Output["finish_reason"].(string); got != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", got)
	}
	if got, _ := result.Output["content"].(string); got != "" {
		t.Errorf("content = %q, want empty", got)
	}
	if len(stream.chunks) != 0 {
		t.Errorf("chunks = %v, want none (no delta.content in any frame)", stream.chunks)
	}
	raw, ok := result.Output["tool_calls"].(json.RawMessage)
	if !ok {
		t.Fatalf("Output[\"tool_calls\"] = %#v (%T), want json.RawMessage", result.Output["tool_calls"], result.Output["tool_calls"])
	}
	var parsed []map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("tool_calls did not unmarshal as an array: %v", err)
	}
	if len(parsed) != 1 {
		t.Fatalf("tool_calls = %v, want exactly one merged entry", parsed)
	}
	if parsed[0]["id"] != "call_1" {
		t.Errorf("tool_calls[0].id = %v, want call_1", parsed[0]["id"])
	}
	fn, _ := parsed[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("tool_calls[0].function.name = %v, want get_weather", fn["name"])
	}
	if fn["arguments"] != `{"city":"SF"}` {
		t.Errorf("tool_calls[0].function.arguments = %v, want merged JSON string", fn["arguments"])
	}
}

// TestLLMInferenceHandler_StreamingTextUnaffectedByToolCallPlumbing is the
// #603 "no tools key ⇒ identical behavior" pin for the streaming path: a
// plain text stream (no tool_calls deltas anywhere) must still report
// finish_reason "stop" exactly as TestLLMInferenceHandler_ChatStreaming
// already asserts, and gain no "tool_calls" key.
func TestLLMInferenceHandler_StreamingTextUnaffectedByToolCallPlumbing(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
		`data: [DONE]`,
	}
	ts := newChatCompletionsServer(t, frames, nil)
	defer ts.Close()

	h := NewLLMInferenceHandler()
	h.baseURLs["bonsai"] = ts.URL

	job := &Job{
		ID:   "job-streaming-text-only",
		Type: JobTypeLLMInference,
		Payload: map[string]any{
			"model":    "bonsai-27b",
			"backend":  "bonsai",
			"stream":   true,
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		},
	}
	stream := &MockStreamWriter{}
	result, err := h.Execute(context.Background(), job, stream)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result == nil || result.Status != JobStatusSuccess {
		t.Fatalf("result = %+v, want success", result)
	}
	// finish_reason stays "stop" even though the upstream sent "length" --
	// this streaming path has never forwarded a real finish_reason other than
	// "tool_calls" (see streamChatCompletions's gate), so a text-only stream's
	// output is unchanged by #603.
	if got, _ := result.Output["finish_reason"].(string); got != "stop" {
		t.Errorf("finish_reason = %q, want stop (unchanged pre-#603 behavior)", got)
	}
	if _, present := result.Output["tool_calls"]; present {
		t.Errorf("Output = %+v, want no \"tool_calls\" key", result.Output)
	}
}

// TestLLMInferenceHandler_GroundingGuardrailHandlesToolCallsOnlyResponse pins
// the #603 grounding-guardrail interaction explicitly asked for: a
// tool_calls-only response (empty content) must not crash the guardrail or
// false-flag anything -- it is vacuously grounded because there is no text to
// check (internal/trust.CheckGrounding's ClaimsChecked==0 case).
func TestLLMInferenceHandler_GroundingGuardrailHandlesToolCallsOnlyResponse(t *testing.T) {
	t.Setenv(groundingGuardrailEnvVar, "1")

	body := `{"choices":[{"message":{"content":"","tool_calls":[` +
		`{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}` +
		`]},"finish_reason":"tool_calls"}]}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveReadinessProbe(w, r) {
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	h := NewLLMInferenceHandler()
	h.baseURLs["bonsai"] = ts.URL

	job := &Job{
		ID:   "job-grounding-tool-calls",
		Type: JobTypeLLMInference,
		Payload: map[string]any{
			"model":    "m",
			"backend":  "bonsai",
			"messages": []map[string]any{{"role": "user", "content": "weather in SF?"}},
		},
	}
	result, err := h.Execute(context.Background(), job, &MockStreamWriter{})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result == nil || result.Status != JobStatusSuccess {
		t.Fatalf("result = %+v, want success", result)
	}
	grounding, ok := result.Output["grounding"].(map[string]any)
	if !ok {
		t.Fatalf("Output[\"grounding\"] = %#v, want map[string]any", result.Output["grounding"])
	}
	if grounded, _ := grounding["grounded"].(bool); !grounded {
		t.Errorf("grounding[\"grounded\"] = %v, want true (nothing to check)", grounded)
	}
	if checked, _ := grounding["claims_checked"].(int); checked != 0 {
		t.Errorf("grounding[\"claims_checked\"] = %v, want 0", grounding["claims_checked"])
	}
	if _, present := result.Output["tool_calls"]; !present {
		t.Errorf("Output = %+v, want tool_calls to still be present alongside grounding", result.Output)
	}
}

// TestLLMInferenceHandler_BufferedReasoningFallbackSkippedWhenToolCallsPresent
// pins the advisor-caught collision between the thinking-model reasoning_content
// fallback and tool_calls: a response with empty content, non-empty
// reasoning_content, AND tool_calls must NOT promote the chain-of-thought to
// `content` -- doing so would render the CoT as a bogus visible assistant
// reply alongside the tool call.
func TestLLMInferenceHandler_BufferedReasoningFallbackSkippedWhenToolCallsPresent(t *testing.T) {
	body := `{"choices":[{"message":{"content":"","reasoning_content":"I should check the weather API",` +
		`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},` +
		`"finish_reason":"tool_calls"}]}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveReadinessProbe(w, r) {
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	h := NewLLMInferenceHandler()
	h.baseURLs["bonsai"] = ts.URL

	job := &Job{
		ID:   "job-reasoning-vs-tool-calls",
		Type: JobTypeLLMInference,
		Payload: map[string]any{
			"model":    "m",
			"backend":  "bonsai",
			"messages": []map[string]any{{"role": "user", "content": "weather in SF?"}},
		},
	}
	result, err := h.Execute(context.Background(), job, &MockStreamWriter{})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result == nil || result.Status != JobStatusSuccess {
		t.Fatalf("result = %+v, want success", result)
	}
	if got, _ := result.Output["content"].(string); got != "" {
		t.Errorf("content = %q, want empty (reasoning must not leak into content when tool_calls present)", got)
	}
	if _, present := result.Output["tool_calls"]; !present {
		t.Errorf("Output = %+v, want tool_calls present", result.Output)
	}
}

// TestLLMInferenceHandler_BufferedContentAndToolCallsBothPresent covers the
// OpenAI-permitted "narration + tool call" shape (e.g. "Let me check the
// weather" alongside a tool_calls array) — real, non-empty `content` must be
// preserved verbatim (NOT zeroed out just because tool_calls are also
// present) and still publish its parity chunk, while `tool_calls` also
// attaches. This is the case advisor review flagged as needing an explicit
// pin: a future "simplify the branch to `if len(msg.ToolCalls) > 0 { content
// = "" }`" edit would pass every other tool_calls test but fail this one.
func TestLLMInferenceHandler_BufferedContentAndToolCallsBothPresent(t *testing.T) {
	body := `{"choices":[{"message":{"content":"Let me check the weather.",` +
		`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},` +
		`"finish_reason":"tool_calls"}]}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveReadinessProbe(w, r) {
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	h := NewLLMInferenceHandler()
	h.baseURLs["bonsai"] = ts.URL

	job := &Job{
		ID:   "job-content-and-tool-calls",
		Type: JobTypeLLMInference,
		Payload: map[string]any{
			"model":    "m",
			"backend":  "bonsai",
			"messages": []map[string]any{{"role": "user", "content": "weather in SF?"}},
		},
	}
	stream := &MockStreamWriter{}
	result, err := h.Execute(context.Background(), job, stream)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result == nil || result.Status != JobStatusSuccess {
		t.Fatalf("result = %+v, want success", result)
	}
	if got, _ := result.Output["content"].(string); got != "Let me check the weather." {
		t.Errorf("content = %q, want the narration text preserved", got)
	}
	if _, present := result.Output["tool_calls"]; !present {
		t.Errorf("Output = %+v, want tool_calls present alongside content", result.Output)
	}
	if len(stream.chunks) != 1 || stream.chunks[0] != "Let me check the weather." {
		t.Errorf("chunks = %v, want the narration published as a single parity chunk", stream.chunks)
	}
}

// TestLLMInferenceHandler_ToolCallsNullOrEmptyTreatedAsAbsent is the coordinator
// review pin for a real robustness gap: json.RawMessage's byte length alone is
// NOT a meaningful "tool calls present" check -- the literal JSON `null` is 4
// bytes and `[]` is 2 bytes, both len(raw) > 0 despite meaning "no tool calls".
// A Pydantic-typed engine response can plausibly serialize an explicit
// "tool_calls": null (or []) on an ORDINARY reply. Before the hasToolCalls fix,
// a thinking model (e.g. bonsai) with empty content, non-empty
// reasoning_content, AND an explicit null/empty tool_calls field would take the
// "tool_calls present" branch and NOT fall back to reasoning_content -- the
// caller got an empty reply instead of the reasoning. This must FAIL against
// the old `len(msg.ToolCalls) > 0` check.
func TestLLMInferenceHandler_ToolCallsNullOrEmptyTreatedAsAbsent(t *testing.T) {
	cases := []struct {
		name         string
		toolCallsRaw string
	}{
		{name: "explicit null", toolCallsRaw: `null`},
		{name: "empty array", toolCallsRaw: `[]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"choices":[{"message":{"content":"","reasoning_content":"thinking hard about the weather",` +
				`"tool_calls":` + tc.toolCallsRaw + `},"finish_reason":"stop"}]}`
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveReadinessProbe(w, r) {
					return
				}
				_, _ = w.Write([]byte(body))
			}))
			defer ts.Close()

			h := NewLLMInferenceHandler()
			h.baseURLs["bonsai"] = ts.URL

			job := &Job{
				ID:   "job-null-tool-calls-" + tc.name,
				Type: JobTypeLLMInference,
				Payload: map[string]any{
					"model":    "m",
					"backend":  "bonsai",
					"messages": []map[string]any{{"role": "user", "content": "weather in SF?"}},
				},
			}
			stream := &MockStreamWriter{}
			result, err := h.Execute(context.Background(), job, stream)
			if err != nil {
				t.Fatalf("Execute error: %v", err)
			}
			if result == nil || result.Status != JobStatusSuccess {
				t.Fatalf("result = %+v, want success", result)
			}
			if got, _ := result.Output["content"].(string); got != "thinking hard about the weather" {
				t.Errorf("content = %q, want the reasoning fallback to fire (tool_calls=%s must behave as absent)", got, tc.toolCallsRaw)
			}
			if _, present := result.Output["tool_calls"]; present {
				t.Errorf("Output = %+v, want no \"tool_calls\" key (tool_calls=%s means absent)", result.Output, tc.toolCallsRaw)
			}
			if len(stream.chunks) != 1 || stream.chunks[0] != "thinking hard about the weather" {
				t.Errorf("chunks = %v, want the reasoning published as a single parity chunk", stream.chunks)
			}
		})
	}
}

// TestLLMInferenceHandler_RequestSideNullToolCallsNotForwarded is the
// request-build analogue of the response-side fix above: an assistant
// message in the incoming payload carrying an explicit "tool_calls": null (or
// []) must not gain a spurious "tool_calls" key on the outbound engine
// request -- it must behave exactly like an absent field.
func TestLLMInferenceHandler_RequestSideNullToolCallsNotForwarded(t *testing.T) {
	cases := []struct {
		name          string
		toolCallsJSON json.RawMessage
	}{
		{name: "explicit null", toolCallsJSON: json.RawMessage(`null`)},
		{name: "empty array", toolCallsJSON: json.RawMessage(`[]`)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotReq map[string]any
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveReadinessProbe(w, r) {
					return
				}
				_ = json.NewDecoder(r.Body).Decode(&gotReq)
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
			}))
			defer ts.Close()

			h := NewLLMInferenceHandler()
			h.baseURLs["bonsai"] = ts.URL

			// Build the payload directly (not via a job.Payload map[string]any
			// round-trip) so the ChatMessage.ToolCalls field is exactly the raw
			// literal under test, matching how parseLLMInferencePayload would
			// decode an engine/backend that actually sent this literal.
			payload := &jobs.LLMInferencePayload{
				Model:   "m",
				Backend: "bonsai",
				Messages: []jobs.ChatMessage{
					{Role: "user", Content: json.RawMessage(`"hi"`)},
					{Role: "assistant", Content: json.RawMessage(`"ok"`), ToolCalls: tc.toolCallsJSON},
				},
			}
			result, err := h.executeLlamaCppAt(context.Background(), &MockStreamWriter{}, payload, ts.URL, "job-null-tool-calls-req-"+tc.name)
			if err != nil {
				t.Fatalf("executeLlamaCppAt error: %v", err)
			}
			if result == nil || result.Status != JobStatusSuccess {
				t.Fatalf("result = %+v, want success", result)
			}

			msgs, _ := gotReq["messages"].([]any)
			if len(msgs) != 2 {
				t.Fatalf("outbound messages = %v, want 2", msgs)
			}
			assistantMsg, _ := msgs[1].(map[string]any)
			if _, present := assistantMsg["tool_calls"]; present {
				t.Errorf("outbound assistant message = %+v, want no \"tool_calls\" key (input was %s)", assistantMsg, tc.toolCallsJSON)
			}
		})
	}
}
