package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

func TestContainerMediaDeleteSessionValidatesStatus(t *testing.T) {
	for _, tc := range []struct {
		status  int
		wantErr bool
	}{
		{http.StatusNoContent, false},
		{http.StatusNotFound, false}, // already reaped is idempotent success
		{http.StatusInternalServerError, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete || r.URL.Path != "/sessions/m1" {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":"teardown failed"}`))
		}))
		m := newTestContainerMedia(srv.URL)
		err := m.deleteSession()
		srv.Close()
		if (err != nil) != tc.wantErr {
			t.Errorf("DELETE status %d error = %v, wantErr=%v", tc.status, err, tc.wantErr)
		}
	}
}

func TestContainerMediaMalformedCreatedResponseCleansSession(t *testing.T) {
	var deletes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"session_id":`))
		case http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	m := newTestContainerMedia(srv.URL)
	err := m.createSession()
	if err == nil || !strings.Contains(err.Error(), "parse meetingd session response") {
		t.Fatalf("createSession error = %v, want malformed-response failure", err)
	}
	if deletes.Load() != 1 {
		t.Fatalf("DELETE calls = %d, want cleanup of created deterministic session", deletes.Load())
	}
}

func TestContainerMediaCreatedResponseReadFailureCleansSession(t *testing.T) {
	var deletes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("test server does not support hijacking")
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		_, _ = fmt.Fprint(rw, "HTTP/1.1 201 Created\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"session_id\":")
		_ = rw.Flush()
		_ = conn.Close()
	}))
	defer srv.Close()
	m := newTestContainerMedia(srv.URL)
	err := m.createSession()
	if err == nil || !strings.Contains(err.Error(), "meetingd create session") {
		t.Fatalf("createSession error = %v, want response-read failure", err)
	}
	if deletes.Load() != 1 {
		t.Fatalf("DELETE calls = %d, want cleanup after ambiguous 201 read failure", deletes.Load())
	}
}

func TestContainerMediaReadyCancellationCleansSession(t *testing.T) {
	deletes := make(chan struct{}, 1)
	meetingd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"session_id":"m1","cdp_port":9223}`))
		case http.MethodDelete:
			deletes <- struct{}{}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer meetingd.Close()
	probeStarted := make(chan struct{}, 1)
	cdp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeStarted <- struct{}{}
		<-r.Context().Done()
	}))
	defer cdp.Close()

	m := newTestContainerMedia(meetingd.URL)
	m.cdpPort = cdp.Listener.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := m.StartContext(ctx)
		result <- err
	}()
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("CDP readiness probe did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("StartContext error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("CDP readiness did not wake promptly on cancellation")
	}
	select {
	case <-deletes:
	case <-time.After(time.Second):
		t.Fatal("cancelled CDP readiness did not release meetingd session")
	}
}

func TestMeetingdHealthyContextHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if meetingdHealthyContext(ctx, &http.Client{Timeout: time.Second}, "http://127.0.0.1:1") {
		t.Fatal("cancelled meetingd health check reported healthy")
	}
}

func TestContainerMediaSpeakPCMUsesSessionScopedRouteAndCancellation(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Query().Get("rate") != "24000" || r.URL.Query().Get("channels") != "1" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusOK)
	}))
	m := newTestContainerMedia(srv.URL)
	m.client = srv.Client()
	if err := m.SpeakPCM(context.Background(), []byte{0, 0}, 24000, 1); err != nil {
		t.Fatalf("SpeakPCM: %v", err)
	}
	srv.Close()
	if gotPath != "/sessions/m1/mic/play/pcm" {
		t.Fatalf("path = %q, want session-scoped microphone route", gotPath)
	}

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer blocked.Close()
	defer close(release)
	m = newTestContainerMedia(blocked.URL)
	m.client = blocked.Client()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.SpeakPCM(ctx, []byte{0, 0}, 24000, 1) }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SpeakPCM error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SpeakPCM did not stop on context cancellation")
	}
}

func TestContainerMediaSpeakPCMOnlyTreatsActivePlaybackAsBusy(t *testing.T) {
	for _, tc := range []struct {
		body     string
		wantBusy bool
	}{
		{body: `{"error":"already speaking"}`, wantBusy: true},
		{body: `{"error":"meeting session ended"}`, wantBusy: false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(tc.body))
		}))
		m := newTestContainerMedia(srv.URL)
		err := m.SpeakPCM(context.Background(), []byte{0, 0}, 24000, 1)
		srv.Close()
		if errors.Is(err, errMicBusy) != tc.wantBusy {
			t.Fatalf("body %s error = %v, wantBusy=%v", tc.body, err, tc.wantBusy)
		}
	}
}

func TestContainerMediaStopSpeakingUsesExactSessionRoute(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"stopped":true}`))
	}))
	defer srv.Close()
	m := newTestContainerMedia(srv.URL)
	m.client = srv.Client()
	if err := m.StopSpeaking(context.Background()); err != nil {
		t.Fatalf("StopSpeaking: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/sessions/m1/mic/stop" {
		t.Fatalf("request = %s %s, want POST /sessions/m1/mic/stop", gotMethod, gotPath)
	}
}

func TestContainerMediaCaptureStreamUsesSessionAndStopsOnCancel(t *testing.T) {
	started := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{1, 2})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	m := newTestContainerMedia(srv.URL)
	m.client = srv.Client()
	ctx, cancel := context.WithCancel(context.Background())
	body, err := m.CaptureStream(ctx, 24000, 1)
	if err != nil {
		t.Fatalf("CaptureStream: %v", err)
	}
	if uri := <-started; uri != "/sessions/m1/capture/pcm?rate=24000&channels=1" {
		t.Fatalf("capture URI = %q", uri)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(body, buf); err != nil {
		t.Fatalf("read capture: %v", err)
	}
	cancel()
	_ = body.Close()
}

// fakeMedia is a MeetingMedia stub for selection tests.
type fakeMedia struct{}

func (fakeMedia) Start() (meetingBrowser, error)  { return nil, nil }
func (fakeMedia) StartRecording() error           { return nil }
func (fakeMedia) StopRecording() (string, error)  { return "", nil }
func (fakeMedia) Close() error                    { return nil }
func (fakeMedia) RecordingAlive() <-chan struct{} { return nil }

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

// TestContainerMediaSpeakFile exercises the bot->room speaking transport
// (aceteam#7079): SpeakFile POSTs the workspace-relative path as JSON to meetingd's
// /sessions/{id}/mic/play and treats a 200 as success. Verified without a real container/pulse.
func TestContainerMediaSpeakFile(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sessions/m1/mic/play" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"played":true,"source":"file","path":"/workspace/tts/hi.wav"}`))
	}))
	defer srv.Close()

	m := newTestContainerMedia(srv.URL)
	if err := m.SpeakFile("tts/hi.wav"); err != nil {
		t.Fatalf("SpeakFile: %v", err)
	}
	if gotBody["path"] != "tts/hi.wav" {
		t.Errorf("posted path = %v, want the workspace-relative tts/hi.wav", gotBody["path"])
	}
}

// TestContainerMediaSpeakFileErrorStatus: a non-200 (e.g. 409 already speaking, or
// 503 mic absent) surfaces as an error with the status, not a silent success.
func TestContainerMediaSpeakFileErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"already speaking"}`))
	}))
	defer srv.Close()

	m := newTestContainerMedia(srv.URL)
	err := m.SpeakFile("tts/hi.wav")
	if err == nil {
		t.Fatal("SpeakFile returned nil error on a 409, want an error")
	}
	if !strings.Contains(err.Error(), "409") {
		t.Errorf("error = %q, want it to mention the 409 status", err.Error())
	}
}
