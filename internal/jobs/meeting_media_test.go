package jobs

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestContainerMedia builds a containerMedia pointed at a mock meetingd, so
// the HTTP control contract is exercised without a real container or CDP socket.
func newTestContainerMedia(base string) *containerMedia {
	return &containerMedia{
		wavRelPath:  "meetings/m1.wav",
		wavAbsPath:  "/ws/meetings/m1.wav",
		maxDuration: time.Hour,
		base:        base,
		cdpPort:     8208,
		client:      &http.Client{Timeout: 5 * time.Second},
		sessionID:   "m1",
	}
}

func TestContainerMediaCreateSession(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sessions" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusCreated)
		// meetingd reports the CONTAINER-internal cdp_port (9223); the client must
		// ignore it and keep using the published host port.
		_, _ = w.Write([]byte(`{"session_id":"srv-assigned","cdp_port":9223,"sink":"citadel_meeting_m1"}`))
	}))
	defer srv.Close()

	m := newTestContainerMedia(srv.URL)
	if err := m.createSession(); err != nil {
		t.Fatalf("createSession: %v", err)
	}
	if m.sessionID != "srv-assigned" {
		t.Errorf("sessionID = %q, want the server-assigned id", m.sessionID)
	}
	if m.cdpPort != 8208 {
		t.Errorf("cdpPort = %d, want the published host port 8208 (not the reported container port 9223)", m.cdpPort)
	}
	if got, ok := gotBody["max_duration_seconds"].(float64); !ok || int(got) != 3600 {
		t.Errorf("max_duration_seconds = %v, want 3600", gotBody["max_duration_seconds"])
	}
	if gotBody["session_id"] != "m1" {
		t.Errorf("session_id sent = %v, want the deterministic id m1", gotBody["session_id"])
	}
}

func TestContainerMediaCreateSessionConflictRetriesAfterDelete(t *testing.T) {
	var mu sync.Mutex
	var posts, deletes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sessions":
			posts++
			if posts == 1 {
				// A stale session is active: 409 the first attempt.
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"a meeting session is already active"}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"session_id":"m1","cdp_port":9223,"sink":"s"}`))
		case r.Method == http.MethodDelete:
			deletes++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ended":true}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	m := newTestContainerMedia(srv.URL)
	if err := m.createSession(); err != nil {
		t.Fatalf("createSession after conflict: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 2 {
		t.Errorf("POST /sessions attempts = %d, want 2 (initial 409 + retry)", posts)
	}
	if deletes != 1 {
		t.Errorf("DELETE attempts = %d, want 1 (clear the stale session before retry)", deletes)
	}
}

func TestContainerMediaCreateSessionPersistentConflictErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"a meeting session is already active"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := newTestContainerMedia(srv.URL)
	err := m.createSession()
	if err == nil {
		t.Fatal("expected an error when a different meeting occupies the node, got nil")
	}
	if !strings.Contains(err.Error(), "busy") {
		t.Errorf("error = %v, want a clear busy message", err)
	}
}

func TestContainerMediaRecordRoundTrip(t *testing.T) {
	var recordOut string
	var stopHit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sessions/m1/record":
			var body map[string]any
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			recordOut, _ = body["out"].(string)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"recording":true,"path":"/workspace/meetings/m1.wav"}`))
		case "/sessions/m1/record/stop":
			stopHit = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"recording":false,"path":"/workspace/meetings/m1.wav"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	m := newTestContainerMedia(srv.URL)
	if err := m.StartRecording(); err != nil {
		t.Fatalf("StartRecording: %v", err)
	}
	if recordOut != "meetings/m1.wav" {
		t.Errorf("record out = %q, want the workspace-relative path meetings/m1.wav", recordOut)
	}
	got, err := m.StopRecording()
	if err != nil {
		t.Fatalf("StopRecording: %v", err)
	}
	if !stopHit {
		t.Error("StopRecording did not hit /record/stop")
	}
	// StopRecording returns the host-absolute path the transcriber reads, not
	// meetingd's in-container /workspace path.
	if got != "/ws/meetings/m1.wav" {
		t.Errorf("StopRecording path = %q, want the host-absolute wav path", got)
	}
}

func TestContainerMediaStartRecordingError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no such session"}`))
	}))
	defer srv.Close()
	m := newTestContainerMedia(srv.URL)
	if err := m.StartRecording(); err == nil {
		t.Fatal("expected StartRecording to error on a non-201 status")
	}
}

func TestMeetingdHealthy(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()
	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// meetingd returns 503 when the canary tone probe fails (silent capture).
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unhealthy.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	if !meetingdHealthy(client, healthy.URL) {
		t.Error("meetingdHealthy = false for a 200 /health, want true")
	}
	if meetingdHealthy(client, unhealthy.URL) {
		t.Error("meetingdHealthy = true for a 503 /health, want false")
	}
	if meetingdHealthy(client, "http://127.0.0.1:0") {
		t.Error("meetingdHealthy = true for an unreachable meetingd, want false")
	}
}

// fakeMedia is a MeetingMedia stub for selection tests.
type fakeMedia struct{}

func (fakeMedia) Start() (meetingBrowser, error) { return nil, nil }
func (fakeMedia) StartRecording() error          { return nil }
func (fakeMedia) StopRecording() (string, error) { return "", nil }
func (fakeMedia) Close() error                   { return nil }

func TestSelectMediaUsesOverride(t *testing.T) {
	called := false
	h := &MeetingJoinHandler{
		WorkspaceDir: "/ws",
		newMedia: func(p meetingJoinParams) MeetingMedia {
			called = true
			return fakeMedia{}
		},
	}
	if _, ok := h.selectMedia(meetingJoinParams{MeetingID: "m1"}).(fakeMedia); !ok {
		t.Error("selectMedia did not use the injected newMedia override")
	}
	if !called {
		t.Error("newMedia override was not called")
	}
}

func TestDefaultSelectMediaPicksBackendByHealth(t *testing.T) {
	p := meetingJoinParams{MeetingID: "m1"}

	container := (&MeetingJoinHandler{
		WorkspaceDir:         "/ws",
		containerHealthProbe: func() bool { return true },
	}).defaultSelectMedia(p)
	if _, ok := container.(*containerMedia); !ok {
		t.Errorf("healthy module: got %T, want *containerMedia", container)
	}

	host := (&MeetingJoinHandler{
		WorkspaceDir:         "/ws",
		containerHealthProbe: func() bool { return false },
	}).defaultSelectMedia(p)
	if _, ok := host.(*hostMedia); !ok {
		t.Errorf("unhealthy module: got %T, want *hostMedia (legacy fallback)", host)
	}
}

// TestMeetingWavRelPathMatchesAbs guards the container hand-off invariant: the
// workspace-relative path meetingd writes, joined onto WorkspaceDir, must equal
// the absolute path the transcriber reads. If these ever drift, the container's
// WAV lands where the transcriber cannot find it.
func TestMeetingWavRelPathMatchesAbs(t *testing.T) {
	for _, id := range []string{"m1", "abc-123", "weird/../id", "café"} {
		abs := meetingWavPath("/ws", id)
		joined := filepath.Join("/ws", meetingWavRelPath(id))
		if abs != joined {
			t.Errorf("meetingWavRelPath drift for %q: abs=%q, join(ws,rel)=%q", id, abs, joined)
		}
	}
}
