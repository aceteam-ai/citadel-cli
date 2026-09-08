package jobs

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func TestSynthesizeSpeech_MissingText(t *testing.T) {
	h := NewSynthesizeSpeechHandler()
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "s1",
		Type:    "SYNTHESIZE_SPEECH",
		Payload: map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error for missing text")
	}
}

// TestSynthesizeSpeech_Success drives the handler against a stub kokoro sidecar.
// It must POST the OpenAI-compatible request to /v1/audio/speech, return the
// audio base64-encoded under the "content" marker, and carry the X-TTS-* receipt
// headers back in the "receipt" object.
func TestSynthesizeSpeech_Success(t *testing.T) {
	audioBytes := []byte("OggS-fake-opus-frames")

	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"up","model_loaded":true}`))
			return
		}
		if r.URL.Path == "/v1/audio/speech" {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &gotBody)
			w.Header().Set("Content-Type", "audio/ogg")
			w.Header().Set("X-TTS-Chars", "24")
			w.Header().Set("X-TTS-Duration-Seconds", "3.575")
			w.Header().Set("X-TTS-Model-Version", "kokoro-0.9.4+hexgrad/Kokoro-82M")
			w.Header().Set("X-TTS-Cache-Key", "abc123")
			w.Header().Set("X-TTS-Cache-Hit", "1")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(audioBytes)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	h := NewSynthesizeSpeechHandler()
	h.ServiceURL = srv.URL

	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "s2",
		Type: "SYNTHESIZE_SPEECH",
		Payload: map[string]string{
			"text":  "Hello from the Citadel.",
			"voice": "am_onyx",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The sidecar must receive the text under the OpenAI "input" key, plus voice
	// and a defaulted response_format.
	if gotBody["input"] != "Hello from the Citadel." {
		t.Errorf("forwarded input = %v, want the job text", gotBody["input"])
	}
	if gotBody["voice"] != "am_onyx" {
		t.Errorf("forwarded voice = %v, want am_onyx", gotBody["voice"])
	}
	if gotBody["response_format"] != "opus" {
		t.Errorf("forwarded response_format = %v, want defaulted opus", gotBody["response_format"])
	}

	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}
	if res["encoding"] != "base64" {
		t.Errorf("encoding = %v, want base64", res["encoding"])
	}
	if res["voice"] != "am_onyx" {
		t.Errorf("result voice = %v, want am_onyx", res["voice"])
	}
	if res["format"] != "opus" {
		t.Errorf("result format = %v, want opus", res["format"])
	}
	// The base64 content must decode back to the exact audio bytes.
	content, _ := res["content"].(string)
	decoded, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		t.Fatalf("content is not valid base64: %v", err)
	}
	if string(decoded) != string(audioBytes) {
		t.Errorf("decoded audio = %q, want %q", decoded, audioBytes)
	}

	// The receipt must carry the X-TTS-* metering headers.
	receipt, ok := res["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("receipt missing or wrong type: %v", res["receipt"])
	}
	if receipt["chars"] != float64(24) { // JSON numbers decode to float64
		t.Errorf("receipt chars = %v, want 24", receipt["chars"])
	}
	if receipt["duration_seconds"] != 3.575 {
		t.Errorf("receipt duration_seconds = %v, want 3.575", receipt["duration_seconds"])
	}
	if receipt["model_version"] != "kokoro-0.9.4+hexgrad/Kokoro-82M" {
		t.Errorf("receipt model_version = %v", receipt["model_version"])
	}
	if receipt["cache_key"] != "abc123" {
		t.Errorf("receipt cache_key = %v, want abc123", receipt["cache_key"])
	}
	if receipt["cache_hit"] != true {
		t.Errorf("receipt cache_hit = %v, want true", receipt["cache_hit"])
	}
}

// TestSynthesizeSpeech_InputAlias verifies the OpenAI "input" payload key is
// accepted as an alias for "text".
func TestSynthesizeSpeech_InputAlias(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"up","model_loaded":true}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("audio"))
	}))
	defer srv.Close()

	h := NewSynthesizeSpeechHandler()
	h.ServiceURL = srv.URL

	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "s3",
		Type:    "SYNTHESIZE_SPEECH",
		Payload: map[string]string{"input": "spoken via the input alias"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSynthesizeSpeech_ServiceError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"up","model_loaded":true}`))
			return
		}
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"error":"input too long"}`))
	}))
	defer srv.Close()

	h := NewSynthesizeSpeechHandler()
	h.ServiceURL = srv.URL

	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "s4",
		Type:    "SYNTHESIZE_SPEECH",
		Payload: map[string]string{"text": "x"},
	})
	if err == nil {
		t.Fatal("expected error for non-200 service response")
	}
}

// TestSynthesizeSpeech_WaitForReady_LoadingBodyNotReady pins the cold-start
// gate: kokoro's /health ALWAYS returns 200 and carries readiness in the body
// (model_loaded), so a 200 with model_loaded:false must NOT count as ready. The
// stub flips to loaded after a short delay; waitForReady must keep polling past
// the first (loading) 200 and only return once the body says loaded.
func TestSynthesizeSpeech_WaitForReady_LoadingBodyNotReady(t *testing.T) {
	var mu sync.Mutex
	loaded := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		isLoaded := loaded
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if isLoaded {
			_, _ = w.Write([]byte(`{"status":"up","model_loaded":true}`))
			return
		}
		// Always 200, but not ready yet: readiness is in the body, not the code.
		_, _ = w.Write([]byte(`{"status":"loading","model_loaded":false}`))
	}))
	defer srv.Close()

	go func() {
		time.Sleep(2 * time.Second)
		mu.Lock()
		loaded = true
		mu.Unlock()
	}()

	h := NewSynthesizeSpeechHandler()
	h.ServiceURL = srv.URL

	start := time.Now()
	if err := h.waitForReady(h.ServiceURL); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Fatalf("waitForReady returned after %v; it must keep polling past the loading 200 until model_loaded is true", elapsed)
	}
}

// TestSynthesizeSpeech_WaitForReady_UnreachableFailsFast covers the cold-start
// hang: a sidecar that was never started (nothing listening) must not make
// waitForReady block near the full model-load budget; it should give up within
// the short unreachable window so the backend can fall back to cloud TTS.
func TestSynthesizeSpeech_WaitForReady_UnreachableFailsFast(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("setup: %v", err)
	}

	h := NewSynthesizeSpeechHandler()
	h.ServiceURL = "http://" + addr

	start := time.Now()
	err = h.waitForReady(h.ServiceURL)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for an unreachable sidecar")
	}
	if elapsed > synthesizeUnreachableTimeout+10*time.Second {
		t.Fatalf("waitForReady took %v, want close to the %v fast-fail budget (not the %v patient budget)", elapsed, synthesizeUnreachableTimeout, synthesizeReadyTimeout)
	}
}

// ttsStub returns an httptest server implementing the shared kokoro/omnivoice
// contract (/health always 200 with model_loaded, /v1/audio/speech echoing the
// posted body into gotBody and returning fixed audio bytes with the X-TTS-*
// headers). Used by every backend-routing test below so both the "kokoro"
// (default) and "omnivoice" (citadel-cli#1007) backends can be pointed at
// independent stub servers.
func ttsStub(gotBody *map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"up","model_loaded":true}`))
			return
		}
		if r.URL.Path == "/v1/audio/speech" {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, gotBody)
			w.Header().Set("X-TTS-Model-Version", "stub-1.0")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("audio-bytes"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

// TestSynthesizeSpeech_DefaultBackendRequestByteIdenticalWithoutSpeedOrInstructions
// pins the #1007 rule that every pre-existing kokoro dispatch (no `backend`,
// no `speed`, no `instructions` field) forwards a byte-identical request body:
// `speed` and `instructions` must be ABSENT from the JSON sent to the sidecar,
// not merely empty, mirroring TestLLMInferenceHandler_
// ToolsRequestByteIdenticalWithoutTools's key-absence style (citadel #603).
func TestSynthesizeSpeech_DefaultBackendRequestByteIdenticalWithoutSpeedOrInstructions(t *testing.T) {
	var gotBody map[string]any
	srv := ttsStub(&gotBody)
	defer srv.Close()

	h := &SynthesizeSpeechHandler{BaseURLs: map[string]string{"kokoro": srv.URL, "omnivoice": srv.URL}}

	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "byte-identical",
		Type:    "SYNTHESIZE_SPEECH",
		Payload: map[string]string{"text": "no backend field here"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, present := gotBody["speed"]; present {
		t.Errorf("request body must not carry a 'speed' key when the job payload omits it, got %v", gotBody["speed"])
	}
	if _, present := gotBody["instructions"]; present {
		t.Errorf("request body must not carry an 'instructions' key when the job payload omits it, got %v", gotBody["instructions"])
	}
	if _, present := gotBody["backend"]; present {
		t.Errorf("the OUTGOING sidecar request must not carry a 'backend' key (that selector is citadel-side only), got %v", gotBody["backend"])
	}
	// The three pre-#1007 keys must still be present, unchanged.
	for _, key := range []string{"input", "voice", "response_format"} {
		if _, present := gotBody[key]; !present {
			t.Errorf("request body missing pre-existing key %q", key)
		}
	}
}

// TestSynthesizeSpeech_SpeedAndInstructionsForwarded verifies the new optional
// passthroughs (citadel-cli#1007 §3.4) reach the sidecar when the job payload
// supplies them.
func TestSynthesizeSpeech_SpeedAndInstructionsForwarded(t *testing.T) {
	var gotBody map[string]any
	srv := ttsStub(&gotBody)
	defer srv.Close()

	h := &SynthesizeSpeechHandler{BaseURLs: map[string]string{"kokoro": srv.URL}}

	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "speed-instructions",
		Type: "SYNTHESIZE_SPEECH",
		Payload: map[string]string{
			"text":         "hello",
			"speed":        "1.5",
			"instructions": "warm, gentle, slightly slower pace",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotBody["speed"] != 1.5 {
		t.Errorf("forwarded speed = %v, want 1.5", gotBody["speed"])
	}
	if gotBody["instructions"] != "warm, gentle, slightly slower pace" {
		t.Errorf("forwarded instructions = %v", gotBody["instructions"])
	}
}

// TestSynthesizeSpeech_BackendOmniVoiceRoutesToSecondURL verifies an explicit
// `backend: "omnivoice"` dispatches to the omnivoice entry in BaseURLs, not
// kokoro's, and that the response envelope's "backend" field reflects the
// resolved backend (citadel-cli#1007 §3.4).
func TestSynthesizeSpeech_BackendOmniVoiceRoutesToSecondURL(t *testing.T) {
	var kokoroBody, omnivoiceBody map[string]any
	kokoroSrv := ttsStub(&kokoroBody)
	defer kokoroSrv.Close()
	omnivoiceSrv := ttsStub(&omnivoiceBody)
	defer omnivoiceSrv.Close()

	h := &SynthesizeSpeechHandler{BaseURLs: map[string]string{
		"kokoro":    kokoroSrv.URL,
		"omnivoice": omnivoiceSrv.URL,
	}}

	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "omnivoice-route",
		Type:    "SYNTHESIZE_SPEECH",
		Payload: map[string]string{"text": "route me to omnivoice", "backend": "omnivoice"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if kokoroBody != nil {
		t.Errorf("kokoro stub received a request; it must not be dialed when backend=omnivoice: %v", kokoroBody)
	}
	if omnivoiceBody == nil {
		t.Fatal("omnivoice stub received no request; the omnivoice backend was not dialed")
	}
	if omnivoiceBody["input"] != "route me to omnivoice" {
		t.Errorf("omnivoice forwarded input = %v", omnivoiceBody["input"])
	}

	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}
	if res["backend"] != "omnivoice" {
		t.Errorf("result backend = %v, want omnivoice", res["backend"])
	}
}

// TestSynthesizeSpeech_BackendDefaultsArePerBackend pins the citadel-cli#1007
// reconciliation fix: an omnivoice dispatch that omits voice/response_format
// must default to the OmniVoice adapter's own auto/wav — the adapter is
// wav-only (opus 400s) and rejects am_michael (not one of its presets) — while
// kokoro's omitted-field defaults stay am_michael/opus unchanged. A permissive
// stub accepts anything, so this asserts the FORWARDED body (and the result
// envelope), not merely a 200. Without the per-backend defaults, the shared
// opus/am_michael default would 400 on the first real omnivoice call.
func TestSynthesizeSpeech_BackendDefaultsArePerBackend(t *testing.T) {
	cases := []struct {
		backend    string
		wantVoice  string
		wantFormat string
	}{
		{"omnivoice", "auto", "wav"},
		{"kokoro", "am_michael", "opus"},
	}
	for _, tc := range cases {
		t.Run(tc.backend, func(t *testing.T) {
			var gotBody map[string]any
			srv := ttsStub(&gotBody)
			defer srv.Close()

			h := &SynthesizeSpeechHandler{BaseURLs: map[string]string{tc.backend: srv.URL}}

			out, err := h.Execute(JobContext{}, &nexus.Job{
				ID:      "defaults-" + tc.backend,
				Type:    "SYNTHESIZE_SPEECH",
				Payload: map[string]string{"text": "hi", "backend": tc.backend},
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotBody["voice"] != tc.wantVoice {
				t.Errorf("%s forwarded voice = %v, want %q", tc.backend, gotBody["voice"], tc.wantVoice)
			}
			if gotBody["response_format"] != tc.wantFormat {
				t.Errorf("%s forwarded response_format = %v, want %q", tc.backend, gotBody["response_format"], tc.wantFormat)
			}
			var res map[string]any
			if err := json.Unmarshal(out, &res); err != nil {
				t.Fatalf("result not JSON: %v", err)
			}
			if res["format"] != tc.wantFormat {
				t.Errorf("%s result format = %v, want %q", tc.backend, res["format"], tc.wantFormat)
			}
			if res["voice"] != tc.wantVoice {
				t.Errorf("%s result voice = %v, want %q", tc.backend, res["voice"], tc.wantVoice)
			}
		})
	}
}

// TestSynthesizeSpeech_UnknownBackendFailsWithoutHTTPCall verifies a typo'd or
// unrecognized `backend` value fails the job immediately, WITHOUT ever
// attempting an HTTP call (no health probe, no synthesis POST) -- the same
// explicit-allowlist posture as selfProvisioningEngines
// (model_cache_pull.go): a typo must fail loudly, not silently route
// somewhere or hang on a probe against nothing.
func TestSynthesizeSpeech_UnknownBackendFailsWithoutHTTPCall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := &SynthesizeSpeechHandler{BaseURLs: map[string]string{"kokoro": srv.URL, "omnivoice": srv.URL}}

	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "unknown-backend",
		Type:    "SYNTHESIZE_SPEECH",
		Payload: map[string]string{"text": "hi", "backend": "not-a-real-backend"},
	})
	if err == nil {
		t.Fatal("expected an error for an unrecognized backend")
	}
	if called {
		t.Error("an unrecognized backend must fail WITHOUT ever making an HTTP call")
	}
}
