package jobs

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if _, present := res["words"]; present {
		t.Fatalf("default response added opt-in words: %#v", res["words"])
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
	// The voice-clone passthroughs (aceteam-ai/citadel-services#29) must ALSO be
	// absent when the job payload omits them, so a pre-voice-clone kokoro dispatch
	// is byte-identical to before they existed.
	for _, key := range []string{"engine", "language", "instruct", "word_timestamps"} {
		if _, present := gotBody[key]; present {
			t.Errorf("request body must not carry a %q key when the job payload omits it, got %v", key, gotBody[key])
		}
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

func TestSynthesizeSpeech_CaptionedOptIn(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"up","model_loaded":true}`))
		case "/info":
			_, _ = w.Write([]byte(`{"model_license":"Apache-2.0"}`))
		case "/v1/audio/speech/captioned":
			gotPath = r.URL.Path
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Errorf("decode captioned request: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"audio_base64":"YXVkaW8=","words":[{"word":"hello","start":0.1,"end":0.5}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	h := &SynthesizeSpeechHandler{BaseURLs: map[string]string{"kokoro": srv.URL}}
	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID: "captioned", Type: "SYNTHESIZE_SPEECH",
		Payload: map[string]string{"text": "hello", "speed": "1.5", "word_timestamps": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/audio/speech/captioned" {
		t.Fatalf("captioned request path = %q, want /v1/audio/speech/captioned", gotPath)
	}
	if gotBody["speed"] != 1.5 {
		t.Fatalf("captioned request speed = %#v, want 1.5", gotBody["speed"])
	}

	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result["content"] != "YXVkaW8=" {
		t.Fatalf("captioned content = %#v, want exact encoded audio", result["content"])
	}
	words, ok := result["words"].([]any)
	if !ok {
		t.Fatalf("captioned words type = %T, want []any", result["words"])
	}
	if len(words) != 1 {
		t.Fatalf("captioned words count = %d, want 1", len(words))
	}
	word, ok := words[0].(map[string]any)
	if !ok {
		t.Fatalf("captioned word type = %T, want map[string]any", words[0])
	}
	if got, ok := word["word"].(string); !ok || got != "hello" {
		t.Fatalf("captioned word = %#v, want string hello", word["word"])
	}
	if got, ok := word["start"].(float64); !ok || got != 0.1 {
		t.Fatalf("captioned start = %#v, want float64(0.1)", word["start"])
	}
	if got, ok := word["end"].(float64); !ok || got != 0.5 {
		t.Fatalf("captioned end = %#v, want float64(0.5)", word["end"])
	}
}

func TestSynthesizeSpeech_WordTimestampsFalsePreservesRawContract(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"up","model_loaded":true}`))
			return
		}
		gotPath = r.URL.Path
		_, _ = w.Write([]byte("raw-audio"))
	}))
	defer srv.Close()

	h := &SynthesizeSpeechHandler{BaseURLs: map[string]string{"kokoro": srv.URL}}
	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID: "captioned-false", Type: "SYNTHESIZE_SPEECH",
		Payload: map[string]string{"text": "hello", "word_timestamps": "false"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/audio/speech" {
		t.Fatalf("word_timestamps=false path = %q, want raw speech path", gotPath)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if _, present := result["words"]; present {
		t.Fatalf("word_timestamps=false added words to raw envelope: %#v", result["words"])
	}
}

func TestSynthesizeSpeech_CaptionedNon200PreservesServiceError(t *testing.T) {
	const response = `{"detail":"no alignable spoken words"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"up","model_loaded":true}`))
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(response))
	}))
	defer srv.Close()

	h := &SynthesizeSpeechHandler{BaseURLs: map[string]string{"kokoro": srv.URL}}
	body, err := h.Execute(JobContext{}, &nexus.Job{
		ID: "captioned-no-words", Type: "SYNTHESIZE_SPEECH",
		Payload: map[string]string{"text": "...", "word_timestamps": "true"},
	})
	if err == nil || !strings.Contains(err.Error(), "422 Unprocessable Entity") {
		t.Fatalf("captioned 422 error = %v, want explicit non-200 status", err)
	}
	if string(body) != response {
		t.Fatalf("captioned 422 body = %q, want %q", body, response)
	}
}

func TestSynthesizeSpeech_CaptionedUnsupportedServiceFailsClearly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"up","model_loaded":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	h := &SynthesizeSpeechHandler{BaseURLs: map[string]string{"kokoro": srv.URL}}
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID: "old-service", Type: "SYNTHESIZE_SPEECH",
		Payload: map[string]string{"text": "hello", "word_timestamps": "true"},
	})
	if err == nil || !strings.Contains(err.Error(), "does not support word timestamps") {
		t.Fatalf("expected upgrade hint, got %v", err)
	}
}

func TestSynthesizeSpeech_CaptionedRejectsMalformedWords(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"wrong timestamp type", `{"audio_base64":"YXVkaW8=","words":[{"word":"hello","start":"zero","end":0.5}]}`},
		{"missing timestamp", `{"audio_base64":"YXVkaW8=","words":[{"word":"hello","start":0.1}]}`},
		{"empty word", `{"audio_base64":"YXVkaW8=","words":[{"word":" ","start":0.1,"end":0.5}]}`},
		{"negative timestamp", `{"audio_base64":"YXVkaW8=","words":[{"word":"hello","start":-0.1,"end":0.5}]}`},
		{"reversed interval", `{"audio_base64":"YXVkaW8=","words":[{"word":"hello","start":0.5,"end":0.1}]}`},
		{"overlapping intervals", `{"audio_base64":"YXVkaW8=","words":[{"word":"hello","start":0.1,"end":0.5},{"word":"world","start":0.4,"end":0.8}]}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" {
					_, _ = w.Write([]byte(`{"status":"up","model_loaded":true}`))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			h := &SynthesizeSpeechHandler{BaseURLs: map[string]string{"kokoro": srv.URL}}
			body, err := h.Execute(JobContext{}, &nexus.Job{
				ID: "malformed-captioned", Type: "SYNTHESIZE_SPEECH",
				Payload: map[string]string{"text": "hello", "word_timestamps": "true"},
			})
			if err == nil || !strings.Contains(err.Error(), "invalid captioned speech response") {
				t.Fatalf("malformed captioned response error = %v", err)
			}
			if body != nil {
				t.Fatalf("malformed captioned response returned body %q", body)
			}
		})
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

// voiceCloneStub implements the voice-clone module contract
// (aceteam-ai/citadel-services#29): /health 200 with model_loaded, /info 404
// (recordModelLicense's top-level parse no-ops anyway), and /v1/audio/speech
// capturing the posted body + path. When word_timestamps is requested in the
// body it returns the {audio_base64, mime, words, receipt} JSON the module sends;
// otherwise raw audio bytes. Every response carries the module's X-TTS-* headers,
// including X-TTS-Model-License and the padded-urlsafe-base64 X-TTS-Receipt.
func voiceCloneStub(t *testing.T, gotBody *map[string]any, gotPath *string) *httptest.Server {
	t.Helper()
	// The module's full receipt (padded urlsafe-base64 in X-TTS-Receipt): carries
	// commercial_use/watermark/consent that have no dedicated header.
	fullReceipt := `{"model_license":"CC-BY-NC","engine":"omnivoice","voice_id":"jane-doe","commercial_use":false,"watermark":true,"consent":{"speaker_name":"Jane Doe","attested_by":"jane@example.com","statement":"I consent to this clone","attested_at":"2026-10-09T00:00:00Z"}}`
	receiptHeader := base64.URLEncoding.EncodeToString([]byte(fullReceipt))
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"up","model_loaded":true}`))
			return
		}
		if r.URL.Path == "/info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path == "/v1/audio/speech" {
			if gotPath != nil {
				*gotPath = r.URL.Path
			}
			body, _ := io.ReadAll(r.Body)
			if gotBody != nil {
				_ = json.Unmarshal(body, gotBody)
			}
			var req map[string]any
			_ = json.Unmarshal(body, &req)
			w.Header().Set("X-TTS-Model-Version", "voice-clone-0.1.0")
			w.Header().Set("X-TTS-Model-License", "CC-BY-NC")
			w.Header().Set("X-TTS-Engine", "omnivoice")
			w.Header().Set("X-TTS-Voice-Id", "jane-doe")
			w.Header().Set("X-TTS-Receipt", receiptHeader)
			if wt, _ := req["word_timestamps"].(bool); wt {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"audio_base64":"YXVkaW8=","mime":"audio/mpeg","words":[{"word":"hello","start":0.1,"end":0.5}],"receipt":{}}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("mp3-bytes"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

// TestSynthesizeSpeech_VoiceCloneRoutesToOwnURL verifies backend=voice-clone
// dispatches to the voice-clone entry in BaseURLs (not kokoro/omnivoice), that
// the omitted-field defaults are the module's own auto/mp3, and the result's
// "backend" reflects it.
func TestSynthesizeSpeech_VoiceCloneRoutesToOwnURL(t *testing.T) {
	var kokoroBody, vcBody map[string]any
	kokoroSrv := ttsStub(&kokoroBody)
	defer kokoroSrv.Close()
	vcSrv := voiceCloneStub(t, &vcBody, nil)
	defer vcSrv.Close()

	h := &SynthesizeSpeechHandler{BaseURLs: map[string]string{
		"kokoro":      kokoroSrv.URL,
		"voice-clone": vcSrv.URL,
	}}

	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "vc-route",
		Type:    "SYNTHESIZE_SPEECH",
		Payload: map[string]string{"text": "clone me", "backend": "voice-clone"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kokoroBody != nil {
		t.Errorf("kokoro stub received a request; it must not be dialed when backend=voice-clone: %v", kokoroBody)
	}
	if vcBody == nil {
		t.Fatal("voice-clone stub received no request; the voice-clone backend was not dialed")
	}
	if vcBody["voice"] != "auto" {
		t.Errorf("voice-clone forwarded voice = %v, want defaulted auto", vcBody["voice"])
	}
	if vcBody["response_format"] != "mp3" {
		t.Errorf("voice-clone forwarded response_format = %v, want defaulted mp3", vcBody["response_format"])
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}
	if res["backend"] != "voice-clone" {
		t.Errorf("result backend = %v, want voice-clone", res["backend"])
	}
}

// TestSynthesizeSpeech_VoiceCloneForwardsPassthroughs verifies the new voice-clone
// passthroughs (engine/language/instruct) reach the module when supplied, under
// voice=auto (instruct requires auto on the module side).
func TestSynthesizeSpeech_VoiceCloneForwardsPassthroughs(t *testing.T) {
	var gotBody map[string]any
	srv := voiceCloneStub(t, &gotBody, nil)
	defer srv.Close()

	h := &SynthesizeSpeechHandler{BaseURLs: map[string]string{"voice-clone": srv.URL}}
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "vc-passthroughs",
		Type: "SYNTHESIZE_SPEECH",
		Payload: map[string]string{
			"text":     "design a new voice",
			"backend":  "voice-clone",
			"engine":   "omnivoice",
			"language": "en",
			"instruct": "warm, calm, measured",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotBody["engine"] != "omnivoice" {
		t.Errorf("forwarded engine = %v, want omnivoice", gotBody["engine"])
	}
	if gotBody["language"] != "en" {
		t.Errorf("forwarded language = %v, want en", gotBody["language"])
	}
	if gotBody["instruct"] != "warm, calm, measured" {
		t.Errorf("forwarded instruct = %v", gotBody["instruct"])
	}
}

// TestSynthesizeSpeech_VoiceCloneWordTimestamps pins the per-backend word-timestamps
// routing: voice-clone uses the BASE /v1/audio/speech with a word_timestamps body
// flag (NOT kokoro's /captioned endpoint), and the {audio_base64, words} JSON is
// decoded into the envelope.
func TestSynthesizeSpeech_VoiceCloneWordTimestamps(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	srv := voiceCloneStub(t, &gotBody, &gotPath)
	defer srv.Close()

	h := &SynthesizeSpeechHandler{BaseURLs: map[string]string{"voice-clone": srv.URL}}
	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "vc-words",
		Type: "SYNTHESIZE_SPEECH",
		Payload: map[string]string{
			"text":            "hello",
			"backend":         "voice-clone",
			"voice":           "my-voice",
			"word_timestamps": "true",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/v1/audio/speech" {
		t.Fatalf("voice-clone word_timestamps path = %q, want the base /v1/audio/speech (not /captioned)", gotPath)
	}
	if wt, _ := gotBody["word_timestamps"].(bool); !wt {
		t.Fatalf("voice-clone request must carry word_timestamps:true in the body, got %#v", gotBody["word_timestamps"])
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}
	if res["content"] != "YXVkaW8=" {
		t.Fatalf("content = %#v, want the module's base64 audio", res["content"])
	}
	words, ok := res["words"].([]any)
	if !ok || len(words) != 1 {
		t.Fatalf("words = %#v, want one aligned span", res["words"])
	}
}

// TestSynthesizeSpeech_VoiceCloneReceiptCarriesLicenseAndConsent verifies the
// module's model_license (X-TTS-Model-License) and consent echo (from the
// padded-urlsafe-base64 X-TTS-Receipt) are carried into the job receipt.
func TestSynthesizeSpeech_VoiceCloneReceiptCarriesLicenseAndConsent(t *testing.T) {
	srv := voiceCloneStub(t, nil, nil)
	defer srv.Close()

	h := &SynthesizeSpeechHandler{BaseURLs: map[string]string{"voice-clone": srv.URL}}
	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "vc-receipt",
		Type:    "SYNTHESIZE_SPEECH",
		Payload: map[string]string{"text": "clone me", "backend": "voice-clone", "voice": "my-voice"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}
	receipt, ok := res["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("receipt missing or wrong type: %v", res["receipt"])
	}
	if receipt["model_license"] != "CC-BY-NC" {
		t.Errorf("receipt model_license = %v, want CC-BY-NC", receipt["model_license"])
	}
	if receipt["engine"] != "omnivoice" {
		t.Errorf("receipt engine = %v, want omnivoice", receipt["engine"])
	}
	if receipt["voice_id"] != "jane-doe" {
		t.Errorf("receipt voice_id = %v, want jane-doe", receipt["voice_id"])
	}
	if receipt["commercial_use"] != false {
		t.Errorf("receipt commercial_use = %v, want false", receipt["commercial_use"])
	}
	if receipt["watermark"] != true {
		t.Errorf("receipt watermark = %v, want true", receipt["watermark"])
	}
	consent, ok := receipt["consent"].(map[string]any)
	if !ok {
		t.Fatalf("receipt consent missing or wrong type: %v", receipt["consent"])
	}
	if consent["speaker_name"] != "Jane Doe" {
		t.Errorf("consent speaker_name = %v, want Jane Doe", consent["speaker_name"])
	}
}

// TestSynthesizeSpeech_KokoroReceiptHasNoVoiceCloneKeys pins the additive contract
// from the receipt side: a kokoro response (which sets neither X-TTS-Model-License
// nor X-TTS-Receipt) produces a receipt with NO model_license and NO consent key,
// byte-identical to before those fields existed.
func TestSynthesizeSpeech_KokoroReceiptHasNoVoiceCloneKeys(t *testing.T) {
	var gotBody map[string]any
	srv := ttsStub(&gotBody)
	defer srv.Close()

	h := &SynthesizeSpeechHandler{BaseURLs: map[string]string{"kokoro": srv.URL}}
	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "kokoro-receipt",
		Type:    "SYNTHESIZE_SPEECH",
		Payload: map[string]string{"text": "hi"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}
	receipt, _ := res["receipt"].(map[string]any)
	for _, key := range []string{"model_license", "engine", "voice_id", "commercial_use", "watermark", "consent"} {
		if _, present := receipt[key]; present {
			t.Errorf("kokoro receipt must not carry %q, got %v", key, receipt[key])
		}
	}
}

// TestSynthesizeSpeech_VoiceCloneReceiptMalformedHeaderNoConsentNoError verifies a
// malformed X-TTS-Receipt header degrades to "no consent key" rather than failing
// the job (same best-effort posture as every other receipt field).
func TestSynthesizeSpeech_VoiceCloneReceiptMalformedHeaderNoConsentNoError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"up","model_loaded":true}`))
			return
		}
		if r.URL.Path == "/info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("X-TTS-Model-License", "MIT")
		w.Header().Set("X-TTS-Receipt", "!!!not-base64!!!")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("mp3-bytes"))
	}))
	defer srv.Close()

	h := &SynthesizeSpeechHandler{BaseURLs: map[string]string{"voice-clone": srv.URL}}
	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "vc-bad-receipt",
		Type:    "SYNTHESIZE_SPEECH",
		Payload: map[string]string{"text": "hi", "backend": "voice-clone"},
	})
	if err != nil {
		t.Fatalf("malformed X-TTS-Receipt must not fail the job: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}
	receipt, _ := res["receipt"].(map[string]any)
	if receipt["model_license"] != "MIT" {
		t.Errorf("receipt model_license = %v, want MIT (from its own header)", receipt["model_license"])
	}
	if _, present := receipt["consent"]; present {
		t.Errorf("a malformed X-TTS-Receipt must yield no consent key, got %v", receipt["consent"])
	}
}

// TestSynthesizeSpeech_VoiceClone403SurfacesModuleMessage pins the acceptance: an
// OmniVoice request with the license flag unset returns the module's 403 message,
// which must surface in the job error (not just a bare status).
func TestSynthesizeSpeech_VoiceClone403SurfacesModuleMessage(t *testing.T) {
	const detail = "OmniVoice is disabled; set OMNIVOICE_ACCEPT_NONCOMMERCIAL_LICENSE=true"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"up","model_loaded":true}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"detail":"` + detail + `"}`))
	}))
	defer srv.Close()

	h := &SynthesizeSpeechHandler{BaseURLs: map[string]string{"voice-clone": srv.URL}}
	body, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "vc-403",
		Type:    "SYNTHESIZE_SPEECH",
		Payload: map[string]string{"text": "hi", "backend": "voice-clone", "engine": "omnivoice"},
	})
	if err == nil {
		t.Fatal("expected an error for a 403 license refusal")
	}
	if !strings.Contains(err.Error(), detail) {
		t.Fatalf("403 error must surface the module's message; got %v", err)
	}
	// The module's refusal body must be passed through VERBATIM (aceteam#10823
	// item 4): not swallowed, not rewritten.
	wantBody := `{"detail":"` + detail + `"}`
	if string(body) != wantBody {
		t.Fatalf("403 body must be returned verbatim; got %q, want %q", body, wantBody)
	}
}

// TestSynthesizeSpeech_VoiceCloneInstructionsMapToInstruct pins aceteam#10823
// item 2: the platform's standard `instructions` field is translated to the
// module's `instruct` field for the voice-clone backend, and `instructions` is
// NOT also forwarded.
func TestSynthesizeSpeech_VoiceCloneInstructionsMapToInstruct(t *testing.T) {
	var gotBody map[string]any
	srv := voiceCloneStub(t, &gotBody, nil)
	defer srv.Close()

	h := &SynthesizeSpeechHandler{BaseURLs: map[string]string{"voice-clone": srv.URL}}
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "vc-instructions-map",
		Type: "SYNTHESIZE_SPEECH",
		Payload: map[string]string{
			"text":         "design me",
			"backend":      "voice-clone",
			"instructions": "bright, energetic",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotBody["instruct"] != "bright, energetic" {
		t.Errorf("voice-clone must map instructions -> instruct, got instruct=%v", gotBody["instruct"])
	}
	if _, present := gotBody["instructions"]; present {
		t.Errorf("voice-clone must NOT forward the `instructions` key (the module has no such field), got %v", gotBody["instructions"])
	}
}

// TestSynthesizeSpeech_VoiceCloneEngineOmnivoiceRoutesTo8215 pins aceteam#10823
// item 5: backend=voice-clone + engine=omnivoice dials the voice-clone module
// (8215 in production), NEVER the standalone omnivoice sidecar (8214). The
// `engine` body field is forwarded to the module; it is not the citadel backend
// selector.
func TestSynthesizeSpeech_VoiceCloneEngineOmnivoiceRoutesTo8215(t *testing.T) {
	var omnivoiceBody, vcBody map[string]any
	omnivoiceSrv := ttsStub(&omnivoiceBody)
	defer omnivoiceSrv.Close()
	vcSrv := voiceCloneStub(t, &vcBody, nil)
	defer vcSrv.Close()

	h := &SynthesizeSpeechHandler{BaseURLs: map[string]string{
		"omnivoice":   omnivoiceSrv.URL,
		"voice-clone": vcSrv.URL,
	}}
	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "vc-engine-omnivoice",
		Type:    "SYNTHESIZE_SPEECH",
		Payload: map[string]string{"text": "hi", "backend": "voice-clone", "engine": "omnivoice"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if omnivoiceBody != nil {
		t.Fatalf("the standalone omnivoice sidecar must NOT be dialed; engine=omnivoice is a voice-clone body field: %v", omnivoiceBody)
	}
	if vcBody == nil {
		t.Fatal("the voice-clone module was not dialed")
	}
	if vcBody["engine"] != "omnivoice" {
		t.Errorf("voice-clone request engine = %v, want omnivoice forwarded in the body", vcBody["engine"])
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}
	if res["backend"] != "voice-clone" {
		t.Errorf("result backend = %v, want voice-clone", res["backend"])
	}
}
