package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/meetingpolicy"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

type refusingMedia struct {
	pipelineMedia
	startErr error
}

func (m *refusingMedia) Start() (meetingBrowser, error) { return nil, m.startErr }

func c1Job() *nexus.Job {
	return &nexus.Job{ID: "synthetic", Payload: map[string]string{
		"meeting_url": "https://meet.google.com/synthetic", "meeting_id": "synthetic",
		"bot_display_name": "Recorder", "owner_display_name": "Owner"}}
}

// A definite pre-launch refusal (busy profile, meetingd busy/not ready) owns
// nothing, so it is an ordinary failure: never teardown uncertainty and no close.
func TestMeetingDefiniteStartRefusalIsOrdinaryFailure(t *testing.T) {
	h := NewMeetingJoinHandler(t.TempDir())
	h.diskPressureFn = func(string) bool { return false }
	media := &refusingMedia{startErr: fmt.Errorf("%w: meeting module is busy", ErrMeetingStartRefused)}
	h.newMedia = func(meetingJoinParams) MeetingMedia { return media }
	_, err := h.Execute(JobContext{}, c1Job())
	if err == nil || !errors.Is(err, ErrMeetingStartRefused) || errors.Is(err, ErrMeetingTeardownUnconfirmed) {
		t.Fatalf("want a plain start refusal, got %v", err)
	}
	if media.closed {
		t.Fatal("refused start closed media it never owned")
	}
}

// An ambiguous start failure keeps the original custody rule.
func TestMeetingAmbiguousStartFailureRetainsCustody(t *testing.T) {
	h := NewMeetingJoinHandler(t.TempDir())
	h.diskPressureFn = func(string) bool { return false }
	media := &refusingMedia{startErr: errors.New("chrome exited during launch")}
	h.newMedia = func(meetingJoinParams) MeetingMedia { return media }
	if _, err := h.Execute(JobContext{}, c1Job()); !errors.Is(err, ErrMeetingTeardownUnconfirmed) {
		t.Fatalf("ambiguous start must stay teardown-unconfirmed, got %v", err)
	}
}

// The audio backup draws from the transcription phase, never from the shared
// teardown/publication allowance C: a slow transcode must leave C intact.
func TestMeetingAudioBackupDoesNotSpendCleanupAllowance(t *testing.T) {
	workspace := t.TempDir()
	h := NewMeetingJoinHandler(workspace)
	h.diskPressureFn = func(string) bool { return false }
	h.AudioBackupEnabled = true
	now := time.Now()
	h.now = func() time.Time { return now }
	cleanupNow := time.Now()
	cleanup := NewMeetingCleanup(180 * time.Second)
	cleanup.now = func() time.Time { return cleanupNow }
	var backupDeadline time.Time
	h.runDockerExec = func(ctx context.Context, _ ...string) ([]byte, error) {
		backupDeadline, _ = ctx.Deadline()
		cleanupNow = cleanupNow.Add(10 * time.Minute) // a slow transcode
		return nil, errors.New("synthetic transcode failure")
	}
	browser := &disclosureBrowser{text: meetingpolicy.Announcement("Recorder", "Owner"), confirmed: true, onEnd: func() { now = now.Add(5 * time.Hour) }}
	media := &pipelineMedia{browser: browser, path: filepath.Join(workspace, "meetings", "synthetic.wav")}
	h.newMedia = func(meetingJoinParams) MeetingMedia { return media }
	h.transcribeFinal = func(JobContext, *nexus.Job, string) ([]byte, error) {
		return []byte(`{"text":"synthetic transcript"}`), nil
	}
	out, err := h.Execute(JobContext{Ctx: WithMeetingCleanup(context.Background(), cleanup)}, c1Job())
	var result map[string]any
	if err != nil || json.Unmarshal(out, &result) != nil || result["status"] != "completed" {
		t.Fatal(result, err)
	}
	if backupDeadline.IsZero() {
		t.Fatal("backup did not run")
	}
	if _, finish, err := cleanup.Begin(context.Background()); err != nil {
		t.Fatal("backup exhausted the cleanup allowance needed for publication")
	} else {
		_ = finish()
	}
}

// The real classifiers, not an injected wrapped error: host busy profile,
// missing Chromium, and meetingd busy/not ready/4xx all refuse cleanly; a
// meetingd 5xx and a failed stale-session clear stay unclassified.
func TestMeetingStartRefusalClassifiers(t *testing.T) {
	prevLock, prevChrome := acquireMeetingProfileSetupLockFn, chromiumAvailableFn
	t.Cleanup(func() { acquireMeetingProfileSetupLockFn, chromiumAvailableFn = prevLock, prevChrome })

	chromiumAvailableFn = func() bool { return true }
	acquireMeetingProfileSetupLockFn = func(string) (string, func(), error) {
		return "", nil, fmt.Errorf("meeting profile busy: %w", platform.ErrMeetingProfileBusy)
	}
	if _, err := newHostMedia("m", t.TempDir(), "x.wav").Start(); !errors.Is(err, ErrMeetingStartRefused) {
		t.Fatalf("busy host profile not a definite refusal: %v", err)
	}
	locked := false
	// If a regression ever passes the preflight, never touch the real profile
	// or this box's live meeting sinks.
	prevMark := markMeetingProfileOwnedFn
	t.Cleanup(func() { markMeetingProfileOwnedFn = prevMark })
	markMeetingProfileOwnedFn = func(string) (string, bool, error) { return "", false, errors.New("synthetic stop") }
	sandbox := t.TempDir()
	acquireMeetingProfileSetupLockFn = func(string) (string, func(), error) { locked = true; return sandbox, func() {}, nil }
	chromiumAvailableFn = func() bool { return false }
	if _, err := newHostMedia("m", t.TempDir(), "x.wav").Start(); !errors.Is(err, ErrMeetingStartRefused) || locked {
		t.Fatalf("missing Chromium must refuse before any setup: %v locked=%v", err, locked)
	}

	for _, tc := range []struct {
		post, del int
		refused   bool
	}{
		{http.StatusConflict, http.StatusOK, true},
		{http.StatusServiceUnavailable, http.StatusOK, true},
		{http.StatusUnprocessableEntity, http.StatusOK, true},
		{http.StatusInternalServerError, http.StatusOK, false},
		{http.StatusConflict, http.StatusInternalServerError, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.WriteHeader(tc.post)
				return
			}
			w.WriteHeader(tc.del)
		}))
		err := newTestContainerMedia(srv.URL).createSession()
		srv.Close()
		if err == nil || errors.Is(err, ErrMeetingStartRefused) != tc.refused {
			t.Fatalf("POST %d / DELETE %d: refused=%v, err=%v", tc.post, tc.del, tc.refused, err)
		}
	}
}
