package jobs

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
			w.WriteHeader(http.StatusOK)
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
			w.WriteHeader(http.StatusOK)
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
			w.WriteHeader(http.StatusOK)
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
	err = h.waitForReady()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for an unreachable sidecar")
	}
	if elapsed > synthesizeUnreachableTimeout+10*time.Second {
		t.Fatalf("waitForReady took %v, want close to the %v fast-fail budget (not the %v patient budget)", elapsed, synthesizeUnreachableTimeout, synthesizeReadyTimeout)
	}
}
