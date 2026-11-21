package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/meetingpolicy"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

type pipelineMedia struct {
	browser         meetingBrowser
	path            string
	closed, stopped bool
}

func (m *pipelineMedia) Start() (meetingBrowser, error) { return m.browser, nil }
func (m *pipelineMedia) StartRecording() error {
	return os.WriteFile(m.path, []byte("synthetic audio fixture"), 0600)
}
func (m *pipelineMedia) StopRecording() (string, error)  { m.stopped = true; return m.path, nil }
func (m *pipelineMedia) Close() error                    { m.closed = true; return nil }
func (m *pipelineMedia) RecordingAlive() <-chan struct{} { return nil }

func TestMeetingMaximumRecordingStillGetsSeparateFinalTranscription(t *testing.T) {
	workspace := t.TempDir()
	h := NewMeetingJoinHandler(workspace)
	h.diskPressureFn = func(string) bool { return false }
	now := time.Now()
	h.now = func() time.Time { return now }
	probes := 0
	browser := &disclosureBrowser{text: meetingpolicy.Announcement("Recorder", "Owner"), confirmed: true, onEnd: func() { probes++; now = now.Add(6 * time.Hour) }}
	path := filepath.Join(workspace, "meetings", "synthetic.wav")
	media := &pipelineMedia{browser: browser, path: path}
	h.newMedia = func(meetingJoinParams) MeetingMedia { return media }
	called := false
	h.transcribeFinal = func(ctx JobContext, _ *nexus.Job, got string) ([]byte, error) {
		called = true
		deadline, ok := ctx.Context().Deadline()
		if !ok || time.Until(deadline) < 4*time.Hour-time.Second || time.Until(deadline) > 4*time.Hour {
			t.Error("recording consumed the final transcription allowance")
		}
		if !media.closed || !media.stopped || got != path {
			t.Error("media not finalized before final transcription")
		}
		if _, err := os.Stat(path); err != nil {
			t.Error("audio was not retained")
		}
		return []byte(`{"text":"synthetic transcript"}`), nil
	}
	out, err := h.Execute(JobContext{}, &nexus.Job{ID: "synthetic", Payload: map[string]string{"meeting_url": "https://meet.google.com/synthetic", "meeting_id": "synthetic", "bot_display_name": "Recorder", "owner_display_name": "Owner", "admit_timeout_seconds": "600", "max_duration_seconds": "21600", "timeout_ms": "36900000"}})
	var result map[string]any
	if probes != 1 {
		t.Fatal("recording start moved past admission", probes)
	}
	if json.Unmarshal(out, &result) != nil || err != nil || !called || result["status"] != "completed" || result["end_reason"] != "max_duration_reached" || result["disclosure_posted"] != true {
		t.Fatal(result, err, called)
	}
}
func TestMeetingFinalTranscriptionTimeoutIsTruthfulAndRetainsAudio(t *testing.T) {
	workspace := t.TempDir()
	h := NewMeetingJoinHandler(workspace)
	h.diskPressureFn = func(string) bool { return false }
	h.Timing = meetingpolicy.Default()
	h.Timing.Transcription = 1
	now := time.Now()
	h.now = func() time.Time { return now }
	b := &disclosureBrowser{text: meetingpolicy.Announcement("", ""), onEnd: func() { now = now.Add(4 * time.Hour) }}
	path := filepath.Join(workspace, "meetings", "synthetic.wav")
	media := &pipelineMedia{browser: b, path: path}
	h.newMedia = func(meetingJoinParams) MeetingMedia { return media }
	h.transcribeFinal = func(ctx JobContext, _ *nexus.Job, _ string) ([]byte, error) {
		<-ctx.Context().Done()
		return nil, ctx.Context().Err()
	}
	out, err := h.Execute(JobContext{}, &nexus.Job{ID: "synthetic", Payload: map[string]string{"meeting_url": "https://meet.google.com/synthetic", "meeting_id": "synthetic"}})
	var result map[string]any
	decodeErr := json.Unmarshal(out, &result)
	if err != nil || decodeErr != nil || result["status"] != "recorded_transcription_failed" || result["transcript"] != nil || result["disclosure_posted"] != false {
		t.Fatal(result, err, decodeErr)
	}
	if _, err := os.Stat(path); err != nil || !media.closed {
		t.Fatal("partial outcome lost audio or media cleanup")
	}
}
func TestMeetingPhaseRejectsLateSuccessAndDoesNotStartAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	called := false
	err := checkMeetingPhase(ctx, func() error { called = true; cancel(); return nil })
	if !called || !errors.Is(err, context.Canceled) {
		t.Fatal("late phase success accepted")
	}
	called = false
	if err := checkMeetingPhase(ctx, func() error { called = true; return nil }); !errors.Is(err, context.Canceled) || called {
		t.Fatal("expired phase ran new native operation")
	}
}
