package jobs

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func TestTranscribeAudio_MissingAudioPath(t *testing.T) {
	h := NewTranscribeAudioHandler(t.TempDir())
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "t1",
		Type:    "TRANSCRIBE_AUDIO",
		Payload: map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error for missing audio_path")
	}
}

func TestTranscribeAudio_PathEscapeRejected(t *testing.T) {
	h := NewTranscribeAudioHandler(t.TempDir())
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "t2",
		Type: "TRANSCRIBE_AUDIO",
		Payload: map[string]string{
			"audio_path": "../../etc/passwd",
		},
	})
	if err == nil {
		t.Fatal("expected error for audio_path escaping the workspace")
	}
}

// TestTranscribeAudio_Success drives the handler against a stub whisper sidecar.
// The audio path must resolve inside the workspace; the handler then forwards a
// workspace-relative path to the service and relays the JSON response verbatim.
func TestTranscribeAudio_Success(t *testing.T) {
	dir := t.TempDir()
	// Create the audio file so ValidatePath resolves it.
	audioRel := filepath.Join("recordings", "meeting.webm")
	if err := os.MkdirAll(filepath.Join(dir, "recordings"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, audioRel), []byte("fakeaudio"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/transcribe" {
			body, _ := io.ReadAll(r.Body)
			var req map[string]any
			_ = json.Unmarshal(body, &req)
			gotPath, _ = req["audio_path"].(string)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"text":"hello world","language":"en","segments":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	h := NewTranscribeAudioHandler(dir)
	h.ServiceURL = srv.URL

	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "t3",
		Type: "TRANSCRIBE_AUDIO",
		Payload: map[string]string{
			"audio_path": audioRel,
			"language":   "en",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The sidecar must receive the workspace-RELATIVE path, never the host abs path.
	if gotPath != audioRel {
		t.Errorf("forwarded audio_path = %q, want %q", gotPath, audioRel)
	}

	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}
	if res["text"] != "hello world" {
		t.Errorf("text = %v, want 'hello world'", res["text"])
	}
}

func TestTranscribeAudio_ServiceError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.webm"), []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	h := NewTranscribeAudioHandler(dir)
	h.ServiceURL = srv.URL

	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "t4",
		Type:    "TRANSCRIBE_AUDIO",
		Payload: map[string]string{"audio_path": "a.webm"},
	})
	if err == nil {
		t.Fatal("expected error for non-200 service response")
	}
}
