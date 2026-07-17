package jobs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func TestTranscodeDockerArgs(t *testing.T) {
	t.Run("with uid/gid maps ownership", func(t *testing.T) {
		args := transcodeDockerArgs("citadel-meeting", "/workspace/meetings/m.wav", "/workspace/meetings/m.opus", 24, 1000, 1000)
		got := strings.Join(args, " ")
		want := "exec -u 1000:1000 citadel-meeting ffmpeg -nostdin -y -i /workspace/meetings/m.wav -c:a libopus -b:a 24k -application voip /workspace/meetings/m.opus"
		if got != want {
			t.Errorf("args =\n  %q\nwant\n  %q", got, want)
		}
	})

	t.Run("negative uid omits -u", func(t *testing.T) {
		args := transcodeDockerArgs("citadel-meeting", "/workspace/a.wav", "/workspace/a.opus", 32, -1, -1)
		for i, a := range args {
			if a == "-u" {
				t.Fatalf("expected no -u flag when uid<0, got at %d: %v", i, args)
			}
		}
		if args[0] != "exec" || args[1] != "citadel-meeting" {
			t.Errorf("expected 'exec citadel-meeting ...', got %v", args[:2])
		}
		if !containsSeq(args, []string{"-b:a", "32k"}) {
			t.Errorf("expected bitrate 32k, got %v", args)
		}
	})
}

func TestMeetingOpusPaths(t *testing.T) {
	// The opus paths are siblings of the wav paths (same dir + sanitized name).
	if got, want := meetingOpusRelPath("abc/../x"), "meetings/abc____x.opus"; got != want {
		t.Errorf("meetingOpusRelPath = %q, want %q", got, want)
	}
	if got, want := meetingOpusPath("/ws", "m1"), filepath.Join("/ws", "meetings", "m1.opus"); got != want {
		t.Errorf("meetingOpusPath = %q, want %q", got, want)
	}
	// wav and opus differ only in extension for the same id.
	wav := meetingWavRelPath("m1")
	opus := meetingOpusRelPath("m1")
	if strings.TrimSuffix(wav, ".wav") != strings.TrimSuffix(opus, ".opus") {
		t.Errorf("wav/opus base drifted: %q vs %q", wav, opus)
	}
}

func TestMeetingAudioUploadURL(t *testing.T) {
	got := meetingAudioUploadURL("https://aceteam.ai/", "11111111-2222-3333-4444-555555555555")
	want := "https://aceteam.ai/api/meetings/11111111-2222-3333-4444-555555555555/audio"
	if got != want {
		t.Errorf("uploadURL = %q, want %q", got, want)
	}
}

func TestUploadMeetingAudio_Success(t *testing.T) {
	dir := t.TempDir()
	opus := filepath.Join(dir, "m.opus")
	payload := []byte("OggS-fake-opus-bytes")
	if err := os.WriteFile(opus, payload, 0o600); err != nil {
		t.Fatalf("write opus: %v", err)
	}

	var gotMethod, gotPath, gotAuth, gotCT string
	var gotLen int64
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotLen = r.ContentLength
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"session_id":"sid","audio_document_id":"doc_9","bytes_stored":20}`))
	}))
	defer srv.Close()

	docID, err := uploadMeetingAudio(context.Background(), srv.Client(), srv.URL, "tok_1", "sid-uuid", opus, meetingAudioMaxUploadBytes)
	if err != nil {
		t.Fatalf("uploadMeetingAudio: %v", err)
	}
	if docID != "doc_9" {
		t.Errorf("docID = %q, want doc_9", docID)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/meetings/sid-uuid/audio" {
		t.Errorf("path = %q, want /api/meetings/sid-uuid/audio", gotPath)
	}
	if gotAuth != "Bearer tok_1" {
		t.Errorf("auth = %q, want 'Bearer tok_1'", gotAuth)
	}
	if gotCT != meetingAudioContentType {
		t.Errorf("content-type = %q, want %q", gotCT, meetingAudioContentType)
	}
	if gotLen != int64(len(payload)) {
		t.Errorf("content-length = %d, want %d (must be declared, not chunked)", gotLen, len(payload))
	}
	if string(gotBody) != string(payload) {
		t.Errorf("body = %q, want %q", gotBody, payload)
	}
}

func TestUploadMeetingAudio_BestEffortErrors(t *testing.T) {
	dir := t.TempDir()
	opus := filepath.Join(dir, "m.opus")
	if err := os.WriteFile(opus, []byte("data"), 0o600); err != nil {
		t.Fatalf("write opus: %v", err)
	}

	t.Run("no token", func(t *testing.T) {
		if _, err := uploadMeetingAudio(context.Background(), http.DefaultClient, "https://x", "", "sid", opus, meetingAudioMaxUploadBytes); err == nil {
			t.Error("expected error with empty token")
		}
	})
	t.Run("no base url", func(t *testing.T) {
		if _, err := uploadMeetingAudio(context.Background(), http.DefaultClient, "", "tok", "sid", opus, meetingAudioMaxUploadBytes); err == nil {
			t.Error("expected error with empty base url")
		}
	})
	t.Run("size guard rejects oversize before request", func(t *testing.T) {
		called := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
		defer srv.Close()
		if _, err := uploadMeetingAudio(context.Background(), srv.Client(), srv.URL, "tok", "sid", opus, 1 /*maxBytes*/); err == nil {
			t.Error("expected size-guard error")
		}
		if called {
			t.Error("server must not be hit when the size guard trips")
		}
	})
	t.Run("empty file", func(t *testing.T) {
		empty := filepath.Join(dir, "empty.opus")
		if err := os.WriteFile(empty, nil, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := uploadMeetingAudio(context.Background(), http.DefaultClient, "https://x", "tok", "sid", empty, meetingAudioMaxUploadBytes); err == nil {
			t.Error("expected error for empty file")
		}
	})
	t.Run("missing file", func(t *testing.T) {
		if _, err := uploadMeetingAudio(context.Background(), http.DefaultClient, "https://x", "tok", "sid", filepath.Join(dir, "nope.opus"), meetingAudioMaxUploadBytes); err == nil {
			t.Error("expected error for missing file")
		}
	})
	t.Run("non-201 is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
		}))
		defer srv.Close()
		if _, err := uploadMeetingAudio(context.Background(), srv.Client(), srv.URL, "tok", "sid", opus, meetingAudioMaxUploadBytes); err == nil {
			t.Error("expected error on 403")
		}
	})
}

// TestUploadAudioBackup_Success drives the orchestrator with injected seams: a
// fake docker-exec that writes the opus, and an httptest backend. On 201 it
// returns true and removes the local opus.
func TestUploadAudioBackup_Success(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "meetings"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	meetingID := "sid-uuid"
	opusHost := meetingOpusPath(ws, meetingID)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"audio_document_id":"doc_x"}`))
	}))
	defer srv.Close()

	transcodeCalled := false
	h := &MeetingJoinHandler{
		WorkspaceDir:       ws,
		AudioBackupEnabled: true,
		backupHTTPClient:   srv.Client(),
		backupCreds:        func() (string, string) { return srv.URL, "tok" },
		runDockerExec: func(ctx context.Context, args ...string) ([]byte, error) {
			transcodeCalled = true
			// Simulate the in-container ffmpeg writing the opus to the shared mount.
			if err := os.WriteFile(opusHost, []byte("opus"), 0o600); err != nil {
				return nil, err
			}
			return nil, nil
		},
	}

	ok := h.uploadAudioBackup(JobContext{}, &nexus.Job{ID: "j1"}, meetingJoinParams{MeetingID: meetingID})
	if !ok {
		t.Fatal("uploadAudioBackup should report success")
	}
	if !transcodeCalled {
		t.Error("transcode should have been invoked")
	}
	if _, err := os.Stat(opusHost); !os.IsNotExist(err) {
		t.Errorf("local opus should be removed after a confirmed upload, stat err = %v", err)
	}
}

func TestUploadAudioBackup_TranscodeFailureIsNonFatal(t *testing.T) {
	ws := t.TempDir()
	h := &MeetingJoinHandler{
		WorkspaceDir:       ws,
		AudioBackupEnabled: true,
		backupCreds:        func() (string, string) { return "https://x", "tok" },
		runDockerExec: func(ctx context.Context, args ...string) ([]byte, error) {
			return []byte("ffmpeg: no such container"), context.DeadlineExceeded
		},
	}
	if h.uploadAudioBackup(JobContext{}, &nexus.Job{ID: "j"}, meetingJoinParams{MeetingID: "m"}) {
		t.Error("transcode failure must yield false (no upload)")
	}
}

// TestBackupAndPrune_DisabledSkipsUploadButPrunes verifies the toggle: when
// disabled, no transcode/upload happens, but retention still runs.
func TestBackupAndPrune_DisabledSkipsUploadButPrunes(t *testing.T) {
	ws := t.TempDir()
	meetings := filepath.Join(ws, "meetings")
	if err := os.MkdirAll(meetings, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	oldWav := writeAged(t, meetings, "old.wav", 40*24*time.Hour)
	current := writeAged(t, meetings, "current.wav", 0)

	transcodeCalled := false
	h := &MeetingJoinHandler{
		WorkspaceDir:       ws,
		AudioBackupEnabled: false,
		AudioRetentionAge:  30 * 24 * time.Hour,
		diskPressureFn:     func(string) bool { return false },
		runDockerExec: func(context.Context, ...string) ([]byte, error) {
			transcodeCalled = true
			return nil, nil
		},
	}

	h.backupAndPrune(JobContext{}, &nexus.Job{ID: "j"}, meetingJoinParams{MeetingID: "current"}, current)

	if transcodeCalled {
		t.Error("disabled backup must not transcode")
	}
	assertGone(t, oldWav)    // retention still ran
	assertExists(t, current) // fresh WAV survives age branch
}

func containsSeq(hay, needle []string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
