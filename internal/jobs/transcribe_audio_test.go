package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/status"
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

// TestTranscribeTimeoutForAudioBytes covers the size -> timeout mapping that
// replaced the fixed 30-minute client cap. The field bug was a 43-minute /
// ~83 MB WAV whose transcription ran past 30 minutes; the budget must now scale
// with the audio's real length, floored for tiny clips and ceilinged for
// absurdly large inputs.
func TestTranscribeTimeoutForAudioBytes(t *testing.T) {
	const bytesPerMinute = transcribeBytesPerSecond * 60 // 32000 * 60 = 1.92 MB/min

	cases := []struct {
		name string
		size int64
		want time.Duration
	}{
		{
			// Zero/unknown size falls back to the generous ceiling, never a
			// small default — under-timing is the exact regression being fixed.
			name: "unknown size uses ceiling",
			size: 0,
			want: transcribeMaxRequestTimeout,
		},
		{
			// A short rolling-window clip (~90s of audio) stays at the floor:
			// 90s * 3 = 4.5min > 2min floor, so it is the raw budget here.
			name: "90s rolling clip",
			size: 90 * transcribeBytesPerSecond,
			want: 90 * transcribeSecondsPerAudioSecond * time.Second,
		},
		{
			// A tiny clip is floored so warm-up/jitter never trips it.
			name: "tiny clip floored",
			size: 5 * transcribeBytesPerSecond,
			want: transcribeMinRequestTimeout,
		},
		{
			// The field case: 43 minutes of audio -> ~2h15m budget, comfortably
			// past the 30-minute cap that failed.
			name: "43 minute meeting",
			size: 43 * bytesPerMinute,
			want: 43 * 60 * transcribeSecondsPerAudioSecond * time.Second,
		},
		{
			// A 4-hour meeting (the MEETING_JOIN hard cap) sits right at the
			// ceiling and must NOT be clamped below its real need.
			name: "four hour meeting at ceiling",
			size: 4 * 60 * bytesPerMinute,
			want: transcribeMaxRequestTimeout,
		},
		{
			// Absurdly large input is bounded so a corrupt file can't wedge a
			// worker slot forever.
			name: "oversized input clamped to ceiling",
			size: 100 * 60 * bytesPerMinute,
			want: transcribeMaxRequestTimeout,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := transcribeTimeoutForAudioBytes(tc.size)
			if got != tc.want {
				t.Errorf("transcribeTimeoutForAudioBytes(%d) = %v, want %v", tc.size, got, tc.want)
			}
		})
	}
}

// TestTranscribeTimeout_BatchLongerThanRolling proves requirement 4: the
// end-of-call batch pass over the full recording gets a far longer budget than a
// short rolling-window clip, since both flow through the same handler and are
// discriminated purely by the file size. This is the client-side selector the
// wire test cannot observe (the context deadline never crosses the wire).
func TestTranscribeTimeout_BatchLongerThanRolling(t *testing.T) {
	dir := t.TempDir()

	// A short rolling clip (~90s) vs a long batch recording (~43min), sized so
	// their byte lengths mirror the real recorder format.
	rollingPath := filepath.Join(dir, "rolling.wav")
	if err := os.WriteFile(rollingPath, make([]byte, 90*transcribeBytesPerSecond), 0o644); err != nil {
		t.Fatalf("setup rolling: %v", err)
	}
	batchPath := filepath.Join(dir, "batch.wav")
	if err := os.WriteFile(batchPath, make([]byte, 43*60*transcribeBytesPerSecond), 0o644); err != nil {
		t.Fatalf("setup batch: %v", err)
	}

	h := NewTranscribeAudioHandler(dir)
	rolling := h.requestTimeout(rollingPath)
	batch := h.requestTimeout(batchPath)

	if batch <= rolling {
		t.Fatalf("batch budget %v should exceed rolling budget %v", batch, rolling)
	}
	// The batch pass must clear the fixed 30-minute cap that failed in the field.
	if batch <= 30*time.Minute {
		t.Errorf("batch budget %v does not exceed the old 30m cap", batch)
	}
	// The rolling clip must stay short — the long budget is only for the full file.
	if rolling > 10*time.Minute {
		t.Errorf("rolling budget %v is unexpectedly large", rolling)
	}
}

// TestTranscribeTimeout_MissingFileUsesCeiling verifies the stat-failure
// direction: a missing file yields the generous ceiling, not a small default.
func TestTranscribeTimeout_MissingFileUsesCeiling(t *testing.T) {
	h := NewTranscribeAudioHandler(t.TempDir())
	got := h.requestTimeout(filepath.Join(t.TempDir(), "does-not-exist.wav"))
	if got != transcribeMaxRequestTimeout {
		t.Errorf("requestTimeout(missing) = %v, want ceiling %v", got, transcribeMaxRequestTimeout)
	}
}

// TestTranscribeTimeout_UsesProbedDurationForCompressedAudio reproduces #1101
// without a real codec or node: 4.5 MB of Opus-like bytes can contain 38
// minutes of audio. The PCM fallback would budget only about seven minutes;
// the probe must instead allow the slower-node 3x duration budget.
func TestTranscribeTimeout_UsesProbedDurationForCompressedAudio(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "long.opus")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 4_500_000); err != nil {
		t.Fatal(err)
	}
	h := NewTranscribeAudioHandler(dir)
	h.probeAudioDurationFn = func(got string) (time.Duration, error) {
		if got != path {
			t.Errorf("probe path = %q, want %q", got, path)
		}
		return 38 * time.Minute, nil
	}
	if got, want := h.requestTimeoutForModel(path, "base"), 114*time.Minute; got != want {
		t.Fatalf("compressed 38m budget = %v, want %v", got, want)
	}
	if old := transcribeTimeoutForAudioBytes(4_500_000); old >= 114*time.Minute {
		t.Fatalf("fixture no longer demonstrates PCM under-estimate: byte budget %v", old)
	}
}

func TestTranscribeTimeout_ProbeFailureFallsBackToBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "malformed.opus")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 4_500_000); err != nil {
		t.Fatal(err)
	}
	h := NewTranscribeAudioHandler(dir)
	h.probeAudioDurationFn = func(string) (time.Duration, error) { return 0, errors.New("no duration") }
	if got, want := h.requestTimeoutForModel(path, "base"), transcribeTimeoutForAudioBytes(4_500_000); got != want {
		t.Fatalf("probe failure budget = %v, want byte fallback %v", got, want)
	}
}

func TestTranscribeTimeout_PCMProbeAndByteEstimateAgree(t *testing.T) {
	const duration = 38 * time.Minute
	h := NewTranscribeAudioHandler(t.TempDir())
	h.probeAudioDurationFn = func(string) (time.Duration, error) { return duration, nil }
	// The file need not contain valid audio: the injected probe keeps this test
	// hermetic while its size models the recorder's 16 kHz mono PCM contract.
	path := filepath.Join(t.TempDir(), "recording.wav")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, int64(duration/time.Second)*transcribeBytesPerSecond); err != nil {
		t.Fatal(err)
	}
	if got, want := h.requestTimeout(path), transcribeTimeoutForAudioBytes(int64(duration/time.Second)*transcribeBytesPerSecond); got != want {
		t.Fatalf("PCM duration budget = %v, want byte-equivalent %v", got, want)
	}
}

func TestRequestTimeoutWithJobBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Hour)
	defer cancel()
	if got := requestTimeoutWithJobBudget(ctx, 114*time.Minute); got < 3*time.Hour+59*time.Minute {
		t.Fatalf("job-authorized deadline was shortened to %v", got)
	}
	if got := requestTimeoutWithJobBudget(context.Background(), 114*time.Minute); got != 114*time.Minute {
		t.Fatalf("unbounded job budget = %v, want media budget", got)
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

// ---------------------------------------------------------------------------
// citadel#891: readiness-failure diagnosis + VRAM preflight
// ---------------------------------------------------------------------------

// fakeContendedNodeStatus builds a NodeStatus shaped like the #558/#891
// incident: a nearly-full GPU held almost entirely by a co-resident vllm.
func fakeContendedNodeStatus() *status.NodeStatus {
	var vllmVRAMGB float64 = 21.2
	vllmVRAMBytes := uint64(vllmVRAMGB * float64(1<<30)) // ~21.2GB
	return &status.NodeStatus{
		System: status.SystemMetrics{MemoryTotalGB: 32, MemoryAvailableGB: 4},
		GPU: []status.GPUMetrics{
			{MemoryTotalMB: 24576, MemoryFreeMB: 2458}, // ~2.4GB free of ~24GB
		},
		Services: []status.ServiceInfo{
			{
				Name:      "vllm",
				Status:    status.ServiceStatusRunning,
				Footprint: &status.ServiceFootprint{VRAMBytes: vllmVRAMBytes},
			},
		},
	}
}

// TestTranscribeAudio_WaitForReady_DiagnosisAnnotatesUnreachable is the
// diagnosed-failure-path test: a sidecar that is not listening at all (the
// fast-fail "unreachable" branch of waitForReady, exercised here instead of
// the 120s patient-timeout branch purely so the test stays fast -- both
// branches route through the same annotateReadinessError) must, once a
// status source is wired, produce a structured, diagnosed failure naming the
// free VRAM/RAM and the current top holder -- not the bare error the
// pre-#891 handler produced.
func TestTranscribeAudio_WaitForReady_DiagnosisAnnotatesUnreachable(t *testing.T) {
	// Bind an ephemeral port, then release it immediately: nothing is
	// listening on the resulting address (mirrors TestTranscribeAudio_
	// WaitForReady_UnreachableFailsFast above).
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
	h.collectStatusFn = func() (*status.NodeStatus, error) { return fakeContendedNodeStatus(), nil }

	waitErr := h.waitForReady()
	if waitErr == nil {
		t.Fatal("expected a readiness-failure error")
	}
	msg := waitErr.Error()
	if !strings.Contains(msg, "unreachable") {
		t.Errorf("error %q lost the original readiness-failure message", msg)
	}
	for _, want := range []string{"gpu:", "2.4GB free", "ram:", "vllm 21.2GB"} {
		if !strings.Contains(msg, want) {
			t.Errorf("diagnosed error %q missing %q", msg, want)
		}
	}
}

// TestTranscribeAudio_AnnotateReadinessError_NoDiagnosisWithoutConfigDir pins
// the hermetic/inert case directly against annotateReadinessError (bypassing
// the real polling loop, which would otherwise need the full
// transcribeReadyTimeout to reach the "did not become ready" message): a
// handler with no ConfigDir and no injected collector (every pre-#891
// construction, incl. every other pre-existing test in this file) must
// return the underlying error UNCHANGED -- no status collection is
// attempted, so this can never fail on a machine with no docker/nvidia-smi.
func TestTranscribeAudio_AnnotateReadinessError_NoDiagnosisWithoutConfigDir(t *testing.T) {
	h := NewTranscribeAudioHandler(t.TempDir())
	// ConfigDir and collectStatusFn both left unset.

	orig := errors.New("transcription service did not become ready within 2m0s")
	got := h.annotateReadinessError(orig)
	if got != orig {
		t.Fatalf("annotateReadinessError = %v, want the original error unchanged (no ConfigDir configured)", got)
	}
}

// TestTranscribeAudio_AnnotateReadinessError_DiagnosesFullTimeout exercises
// the "did not become ready" message shape directly (the patient-timeout
// branch of waitForReady) without actually waiting transcribeReadyTimeout.
func TestTranscribeAudio_AnnotateReadinessError_DiagnosesFullTimeout(t *testing.T) {
	h := NewTranscribeAudioHandler(t.TempDir())
	h.collectStatusFn = func() (*status.NodeStatus, error) { return fakeContendedNodeStatus(), nil }

	orig := errors.New("transcription service did not become ready within " + transcribeReadyTimeout.String())
	got := h.annotateReadinessError(orig)
	if got == nil {
		t.Fatal("expected an annotated error")
	}
	if !errors.Is(got, orig) {
		t.Errorf("annotated error does not wrap the original: %v", got)
	}
	msg := got.Error()
	for _, want := range []string{"did not become ready", "gpu:", "2.4GB free", "ram:", "vllm 21.2GB"} {
		if !strings.Contains(msg, want) {
			t.Errorf("diagnosed error %q missing %q", msg, want)
		}
	}
}

// TestTranscribeAudio_AnnotateReadinessError_CollectionFailureLeavesErrorAlone
// proves the fail-open contract: a collector that errors must never mask the
// original readiness failure with a collection error.
func TestTranscribeAudio_AnnotateReadinessError_CollectionFailureLeavesErrorAlone(t *testing.T) {
	h := NewTranscribeAudioHandler(t.TempDir())
	h.collectStatusFn = func() (*status.NodeStatus, error) {
		return nil, errors.New("docker stats: permission denied")
	}

	orig := errors.New("transcription service did not become ready within 2m0s")
	got := h.annotateReadinessError(orig)
	if got != orig {
		t.Fatalf("annotateReadinessError = %v, want the original error unchanged on a collection failure", got)
	}
}

// TestTranscribeAudio_HealthyPathUnaffectedByDiagnosisWiring proves item 3 of
// the task: wiring a status collector (ConfigDir/collectStatusFn) changes
// NOTHING on the happy path -- the collector is never even consulted when the
// sidecar becomes ready normally.
func TestTranscribeAudio_HealthyPathUnaffectedByDiagnosisWiring(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.webm"), []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"ok","language":"en","segments":[]}`))
	}))
	defer srv.Close()

	collectorCalled := false
	h := NewTranscribeAudioHandler(dir)
	h.ServiceURL = srv.URL
	h.collectStatusFn = func() (*status.NodeStatus, error) {
		collectorCalled = true
		return fakeContendedNodeStatus(), nil
	}

	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "t6",
		Type:    "TRANSCRIBE_AUDIO",
		Payload: map[string]string{"audio_path": "a.webm"}, // no vram_mb: preflight is a no-op
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if collectorCalled {
		t.Error("status collector was consulted on the happy path; it must only run on a readiness failure or a declared VRAM budget")
	}
}

// TestTranscribeAudio_VRAMPreflightRefusesConfirmedShortfall pins the
// structured-reason refusal shape: a payload-declared vram_mb the node
// cannot fit produces a *VRAMRefusal with reason "insufficient_vram" BEFORE
// the sidecar is ever contacted.
func TestTranscribeAudio_VRAMPreflightRefusesConfirmedShortfall(t *testing.T) {
	sidecarHit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sidecarHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := NewTranscribeAudioHandler(t.TempDir())
	h.ServiceURL = srv.URL
	h.collectStatusFn = func() (*status.NodeStatus, error) { return fakeContendedNodeStatus(), nil }

	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "t7",
		Type: "TRANSCRIBE_AUDIO",
		Payload: map[string]string{
			"audio_path": "a.webm",
			"vram_mb":    "4000", // needs 4GB; fakeContendedNodeStatus only has ~2.4GB free
		},
	})
	if err == nil {
		t.Fatal("expected a VRAM preflight refusal")
	}
	var refusal *VRAMRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("error = %v (%T), want *VRAMRefusal", err, err)
	}
	if refusal.Reason != ReasonInsufficientVRAM {
		t.Errorf("Reason = %q, want %q", refusal.Reason, ReasonInsufficientVRAM)
	}
	for _, want := range []string{"3.9GB", "2.4GB", "vllm"} {
		if !strings.Contains(refusal.Message, want) {
			t.Errorf("refusal message %q missing %q", refusal.Message, want)
		}
	}
	// err.Error() must itself be the {"reason":...,"message":...} JSON object
	// (the ShellRefusal convention LegacyHandlerAdapter surfaces verbatim).
	var decoded map[string]string
	if jerr := json.Unmarshal([]byte(err.Error()), &decoded); jerr != nil {
		t.Fatalf("err.Error() is not a JSON object: %v (%q)", jerr, err.Error())
	}
	if decoded["reason"] != ReasonInsufficientVRAM {
		t.Errorf("decoded reason = %q, want %q", decoded["reason"], ReasonInsufficientVRAM)
	}
	if sidecarHit {
		t.Error("sidecar was contacted despite a confirmed VRAM shortfall; preflight must refuse before waitForReady")
	}
}

// TestTranscribeAudio_VRAMPreflightProceedsWhenFits confirms a declared
// budget the node CAN satisfy is a pure no-op: the job proceeds normally.
func TestTranscribeAudio_VRAMPreflightProceedsWhenFits(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.webm"), []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write([]byte(`{"text":"ok","segments":[]}`))
	}))
	defer srv.Close()

	h := NewTranscribeAudioHandler(dir)
	h.ServiceURL = srv.URL
	h.collectStatusFn = func() (*status.NodeStatus, error) { return fakeContendedNodeStatus(), nil }

	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "t8",
		Type: "TRANSCRIBE_AUDIO",
		Payload: map[string]string{
			"audio_path": "a.webm",
			"vram_mb":    "1000", // 1GB fits in the ~2.4GB free
		},
	})
	if err != nil {
		t.Fatalf("unexpected refusal for a satisfiable VRAM budget: %v", err)
	}
}

// TestTranscribeAudio_VRAMPreflightFailsOpenOnUnknownVRAM proves the fail-open
// half of the contract: a declared budget with no GPU signal at all (a
// CPU-only/no-nvidia-smi node) must proceed, not refuse on an unknown value.
func TestTranscribeAudio_VRAMPreflightFailsOpenOnUnknownVRAM(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.webm"), []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write([]byte(`{"text":"ok","segments":[]}`))
	}))
	defer srv.Close()

	h := NewTranscribeAudioHandler(dir)
	h.ServiceURL = srv.URL
	// No GPU entries at all -- freeVRAMBytes reports "unknown".
	h.collectStatusFn = func() (*status.NodeStatus, error) {
		return &status.NodeStatus{System: status.SystemMetrics{MemoryTotalGB: 16, MemoryAvailableGB: 8}}, nil
	}

	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "t9",
		Type: "TRANSCRIBE_AUDIO",
		Payload: map[string]string{
			"audio_path": "a.webm",
			"vram_mb":    "999999", // an absurd requirement -- must still proceed
		},
	})
	if err != nil {
		t.Fatalf("expected fail-open (proceed) on an unknown VRAM signal, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// citadel#1045: model selection + denoise/VAD passthrough + no-speech guard
// ---------------------------------------------------------------------------

// TestApplyTranscribeOptions_NoNewParamsByteIdentical is the backward-compat
// pin: a payload with none of the new params must produce EXACTLY the same
// request the pre-#1045 handler sent — {audio_path[, language][, diarize]} and
// nothing else.
func TestApplyTranscribeOptions_NoNewParamsByteIdentical(t *testing.T) {
	req := map[string]any{"audio_path": "rec.wav"}
	if err := applyTranscribeOptions(req, map[string]string{
		"language": "en",
		"diarize":  "true",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]any{"audio_path": "rec.wav", "language": "en", "diarize": true}
	if !reflect.DeepEqual(req, want) {
		t.Errorf("request = %#v, want %#v", req, want)
	}

	// A truly empty options payload adds nothing at all.
	req2 := map[string]any{"audio_path": "rec.wav"}
	if err := applyTranscribeOptions(req2, map[string]string{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(req2) != 1 {
		t.Errorf("empty options must add nothing, got %#v", req2)
	}
	// diarize=="false" (not the literal "true") must add nothing, preserving the
	// exact pre-existing string-compare semantics.
	req3 := map[string]any{"audio_path": "rec.wav"}
	if err := applyTranscribeOptions(req3, map[string]string{"diarize": "false"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := req3["diarize"]; ok {
		t.Errorf(`diarize "false" must not be forwarded, got %#v`, req3)
	}
}

func TestApplyTranscribeOptions_WordTimestampsOptIn(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{value: "true", want: true},
		{value: "false", want: false},
	} {
		req := map[string]any{"audio_path": "clip.wav"}
		if err := applyTranscribeOptions(req, map[string]string{"word_timestamps": tc.value}); err != nil {
			t.Fatal(err)
		}
		actual, present := req["word_timestamps"]
		if present != tc.want {
			t.Errorf("word_timestamps=%s: key presence=%v, want %v", tc.value, present, tc.want)
		}
		if tc.want && actual != true {
			t.Errorf("word_timestamps=true: value=%#v, want bool true", actual)
		}
	}
	req := map[string]any{"audio_path": "clip.wav"}
	if err := applyTranscribeOptions(req, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if _, present := req["word_timestamps"]; present {
		t.Fatal("absent word_timestamps must not be forwarded")
	}
	if err := applyTranscribeOptions(map[string]any{}, map[string]string{"word_timestamps": "perhaps"}); err == nil {
		t.Fatal("expected invalid word_timestamps to fail before dispatch")
	}
}

func TestTranscribeAudio_WordTimestampsWireContract(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "clip.wav"), []byte("fakeaudio"), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path != "/transcribe" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hello","language":"en","segments":[{"start":0,"end":1,"text":"hello","words":[{"word":"hello","start":0.1,"end":0.9,"probability":0.95}]}]}`))
	}))
	defer srv.Close()
	h := NewTranscribeAudioHandler(dir)
	h.ServiceURL = srv.URL

	for _, tc := range []struct {
		name  string
		value string
		want  bool
	}{
		{name: "absent"},
		{name: "false", value: "false"},
		{name: "true", value: "true", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotReq = nil
			payload := map[string]string{"audio_path": "clip.wav"}
			if tc.value != "" {
				payload["word_timestamps"] = tc.value
			}
			out, err := h.Execute(JobContext{}, &nexus.Job{
				ID: "word-timestamps-" + tc.name, Type: "TRANSCRIBE_AUDIO", Payload: payload,
			})
			if err != nil {
				t.Fatal(err)
			}
			value, present := gotReq["word_timestamps"]
			if present != tc.want || (tc.want && value != true) {
				t.Errorf("sidecar word_timestamps = %#v, present=%v, want true=%v", value, present, tc.want)
			}
			var result map[string]any
			if err := json.Unmarshal(out, &result); err != nil {
				t.Fatal(err)
			}
			segments := result["segments"].([]any)
			words := segments[0].(map[string]any)["words"].([]any)
			word := words[0].(map[string]any)
			if word["word"] != "hello" || word["start"] != 0.1 || word["end"] != 0.9 {
				t.Errorf("nested words changed by handler: %#v", words)
			}
		})
	}
}

// TestApplyTranscribeOptions_NewParamsForwarded checks every new param is
// validated and copied onto the request with the right type.
func TestApplyTranscribeOptions_NewParamsForwarded(t *testing.T) {
	req := map[string]any{"audio_path": "rec.wav"}
	err := applyTranscribeOptions(req, map[string]string{
		"model_size":                  "medium",
		"denoise":                     "true",
		"vad_filter":                  "true",
		"condition_on_previous_text":  "false",
		"no_speech_threshold":         "0.7",
		"compression_ratio_threshold": "2.6",
		"logprob_threshold":           "-0.8",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]any{
		"audio_path":                  "rec.wav",
		"model_size":                  "medium",
		"denoise":                     true,
		"vad_filter":                  true,
		"condition_on_previous_text":  false,
		"no_speech_threshold":         0.7,
		"compression_ratio_threshold": 2.6,
		"logprob_threshold":           -0.8,
	}
	if !reflect.DeepEqual(req, want) {
		t.Errorf("request = %#v, want %#v", req, want)
	}
}

// TestApplyTranscribeOptions_Validation covers the reject cases: an
// unrecognized model_size, a non-bool flag, and a non-numeric threshold each
// error rather than being silently ignored or forwarded.
func TestApplyTranscribeOptions_Validation(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]string
	}{
		{"bad model_size", map[string]string{"model_size": "gigantic"}},
		{"bad denoise", map[string]string{"denoise": "yesplease"}},
		{"bad vad_filter", map[string]string{"vad_filter": "maybe"}},
		{"bad no_speech_threshold", map[string]string{"no_speech_threshold": "high"}},
		{"bad logprob_threshold", map[string]string{"logprob_threshold": "low"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := map[string]any{"audio_path": "rec.wav"}
			if err := applyTranscribeOptions(req, tc.payload); err == nil {
				t.Fatalf("expected validation error for %v", tc.payload)
			}
		})
	}
}

// TestApplyTranscribeOptions_AllWhitelistedModelsAccepted guards the whitelist
// against drift: every advertised model size must validate.
func TestApplyTranscribeOptions_AllWhitelistedModelsAccepted(t *testing.T) {
	for _, m := range allowedWhisperModelSizes {
		req := map[string]any{"audio_path": "rec.wav"}
		if err := applyTranscribeOptions(req, map[string]string{"model_size": m}); err != nil {
			t.Errorf("whitelisted model %q rejected: %v", m, err)
		}
		if got := transcribeModelTimeoutFactors[m]; got < 1 {
			t.Errorf("model %q has no timeout factor entry (defaults would apply); add it to keep the budget sane", m)
		}
	}
}

// TestTranscribeTimeoutFor_ModelAware pins the model-aware budget: the empty
// default is byte-identical to the model-agnostic sizing, while a larger model
// gets a strictly larger budget (factor + one-time load allowance) so a short
// clip is not aborted mid-download.
func TestTranscribeTimeoutFor_ModelAware(t *testing.T) {
	// 43-minute recording — comfortably inside [min,max] for base.
	size := int64(43 * 60 * transcribeBytesPerSecond)

	if got, want := transcribeTimeoutFor(size, ""), transcribeTimeoutForAudioBytes(size); got != want {
		t.Errorf("empty model must equal the model-agnostic budget: got %v want %v", got, want)
	}
	if got, want := transcribeTimeoutFor(size, "base"), transcribeTimeoutForAudioBytes(size); got != want {
		t.Errorf("base model must equal the model-agnostic budget: got %v want %v", got, want)
	}

	base := transcribeTimeoutFor(size, "base")
	medium := transcribeTimeoutFor(size, "medium")
	if medium <= base {
		t.Errorf("medium budget %v should exceed base %v", medium, base)
	}

	// The advisor's exact concern: a SHORT clip with a large model must clear
	// the base floor by at least the model's load allowance, so the first-request
	// download+load is not aborted at 2 minutes.
	shortClip := int64(20 * transcribeBytesPerSecond) // ~20s of audio
	shortMedium := transcribeTimeoutFor(shortClip, "medium")
	if shortMedium <= transcribeMinRequestTimeout {
		t.Errorf("short-clip medium budget %v must exceed the %v floor to survive first-load", shortMedium, transcribeMinRequestTimeout)
	}
	if shortMedium < transcribeModelLoadAllowance["medium"] {
		t.Errorf("short-clip medium budget %v must include the model load allowance %v", shortMedium, transcribeModelLoadAllowance["medium"])
	}

	// Still clamped to the ceiling for absurd inputs.
	if got := transcribeTimeoutFor(int64(1000*60*transcribeBytesPerSecond), "large-v3"); got != transcribeMaxRequestTimeout {
		t.Errorf("oversized large-v3 budget = %v, want ceiling %v", got, transcribeMaxRequestTimeout)
	}
}

// TestTranscribeAudio_InvalidModelSizeFailsFast: a bad model_size must error
// BEFORE the sidecar is ever contacted (validation precedes waitForReady).
func TestTranscribeAudio_InvalidModelSizeFailsFast(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.webm"), []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	sidecarHit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sidecarHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := NewTranscribeAudioHandler(dir)
	h.ServiceURL = srv.URL

	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "tm",
		Type: "TRANSCRIBE_AUDIO",
		Payload: map[string]string{
			"audio_path": "a.webm",
			"model_size": "not-a-model",
		},
	})
	if err == nil {
		t.Fatal("expected an error for an invalid model_size")
	}
	if sidecarHit {
		t.Error("sidecar was contacted despite an invalid model_size; validation must fail fast")
	}
}

// TestTranscribeAudio_NewParamsReachSidecar drives the full handler and asserts
// the new params arrive on the wire, and that the relayed result gains the
// additive transcription_guard.
func TestTranscribeAudio_NewParamsReachSidecar(t *testing.T) {
	dir := t.TempDir()
	audioRel := "rec.wav"
	if err := os.WriteFile(filepath.Join(dir, audioRel), []byte("fakeaudio"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	var gotReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotReq)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hello","language":"en","segments":[{"text":"hello","no_speech_prob":0.02,"avg_logprob":-0.2,"compression_ratio":1.1}]}`))
	}))
	defer srv.Close()

	h := NewTranscribeAudioHandler(dir)
	h.ServiceURL = srv.URL

	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "tp",
		Type: "TRANSCRIBE_AUDIO",
		Payload: map[string]string{
			"audio_path":          audioRel,
			"model_size":          "small",
			"denoise":             "true",
			"vad_filter":          "true",
			"no_speech_threshold": "0.65",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotReq["model_size"] != "small" {
		t.Errorf("model_size not forwarded: %#v", gotReq)
	}
	if gotReq["denoise"] != true || gotReq["vad_filter"] != true {
		t.Errorf("denoise/vad_filter not forwarded: %#v", gotReq)
	}
	if gotReq["no_speech_threshold"] != 0.65 {
		t.Errorf("no_speech_threshold not forwarded: %#v", gotReq)
	}

	var res map[string]json.RawMessage
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}
	if _, ok := res["transcription_guard"]; !ok {
		t.Error("result missing additive transcription_guard")
	}
}
