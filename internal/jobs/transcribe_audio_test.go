package jobs

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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

// TestTranscribeAudio_WaitForReady_UnreachableFailsFast covers the cold-start
// hang: a sidecar that was never started (nothing listening on its port) must
// not make waitForReady block anywhere near the full 120s model-load budget.
// It should give up within the short transcribeUnreachableTimeout window so
// the backend's node-local request fails well inside its own ~100s gateway
// timeout, allowing a fall back to cloud transcription.
func TestTranscribeAudio_WaitForReady_UnreachableFailsFast(t *testing.T) {
	// Bind an ephemeral port, then release it immediately: nothing is
	// listening on the resulting address, so dials to it get an immediate
	// connection-refused, mirroring an absent whisper sidecar.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("setup: %v", err)
	}

	h := NewTranscribeAudioHandler(t.TempDir())
	h.ServiceURL = "http://" + addr

	start := time.Now()
	err = h.waitForReady()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for an unreachable sidecar")
	}
	// Generous slack over the fast-fail budget for scheduler jitter, but this
	// must land nowhere near the 120s patient timeout — that's the bug.
	if elapsed > transcribeUnreachableTimeout+10*time.Second {
		t.Fatalf("waitForReady took %v, want close to the %v fast-fail budget (not the %v patient budget)", elapsed, transcribeUnreachableTimeout, transcribeReadyTimeout)
	}
}

// TestTranscribeAudio_WaitForReady_PatientWhileLoading proves the fast-fail
// path for unreachable sidecars does not regress the legitimate warm-up case:
// a sidecar that answers health checks (just not with 200 yet, because its
// model is still loading) must be given the full patient budget, even past
// the point where an unreachable sidecar would have already failed fast.
func TestTranscribeAudio_WaitForReady_PatientWhileLoading(t *testing.T) {
	var mu sync.Mutex
	ready := false
	calls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		isReady := ready
		mu.Unlock()
		if isReady {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	h := NewTranscribeAudioHandler(t.TempDir())
	h.ServiceURL = srv.URL

	// Flip to ready after longer than transcribeUnreachableTimeout, so a pass
	// here proves the "reachable but loading" path survives past the window
	// that would have killed an actually-unreachable sidecar.
	loadDelay := transcribeUnreachableTimeout + 1*time.Second
	go func() {
		time.Sleep(loadDelay)
		mu.Lock()
		ready = true
		mu.Unlock()
	}()

	start := time.Now()
	if err := h.waitForReady(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < loadDelay {
		t.Fatalf("waitForReady returned after %v, want at least %v (it should have kept polling until ready)", elapsed, loadDelay)
	}

	mu.Lock()
	n := calls
	mu.Unlock()
	if n < 2 {
		t.Fatalf("expected multiple health polls while loading, got %d", n)
	}
}

// TestTranscribeAudio_SymlinkedWorkspace guards the workspace-relative path
// computation when the workspace root itself is a symlink. ValidatePath
// resolves the audio path under the SYMLINK-RESOLVED root, so the handler must
// compute the relative path against the resolved root too. A naive
// filepath.Rel(rawWorkspace, validated) would yield spurious "../" prefixes and
// the sidecar would reject the path.
func TestTranscribeAudio_SymlinkedWorkspace(t *testing.T) {
	realDir := t.TempDir()
	// A sibling symlink that points at the real workspace.
	linkDir := filepath.Join(t.TempDir(), "ws-link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("setup symlink: %v", err)
	}

	audioRel := filepath.Join("recordings", "meeting.webm")
	if err := os.MkdirAll(filepath.Join(realDir, "recordings"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realDir, audioRel), []byte("fakeaudio"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		gotPath, _ = req["audio_path"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"ok","language":"en","segments":[]}`))
	}))
	defer srv.Close()

	// Root the handler at the SYMLINKED path, as a real worker would when its
	// workspace is under a symlinked directory.
	h := NewTranscribeAudioHandler(linkDir)
	h.ServiceURL = srv.URL

	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "t5",
		Type:    "TRANSCRIBE_AUDIO",
		Payload: map[string]string{"audio_path": audioRel},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The forwarded path must be clean and workspace-relative, with no "../".
	if gotPath != audioRel {
		t.Errorf("forwarded audio_path = %q, want %q (no leading ../)", gotPath, audioRel)
	}
}
