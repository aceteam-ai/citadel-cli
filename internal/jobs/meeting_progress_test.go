package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/meetingpolicy"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// lobbyBrowser is a fake page that never admits; denied selects the outcome.
type lobbyBrowser struct {
	disclosureBrowser
	denied bool
}

func (b *lobbyBrowser) Evaluate(js string) (any, error) {
	switch js {
	case meetIsAdmittedJS:
		return false, nil
	case meetDeniedJS:
		return b.denied, nil
	case clickButtonByTextOptionalJS(meetJoinButtonLabels):
		return "Ask to join", nil
	}
	return b.disclosureBrowser.Evaluate(js)
}

func progressRecorder() (*[]map[string]any, func(ProgressEvent)) {
	var got []map[string]any
	return &got, NewThrottledProgress(func(m map[string]any) { got = append(got, m) }, nil)
}

func stages(events []map[string]any) []string {
	var out []string
	for _, e := range events {
		out = append(out, e["stage"].(string))
	}
	return out
}

func endedReason(t *testing.T, events []map[string]any) map[string]any {
	t.Helper()
	for _, e := range events {
		if e["stage"] == MeetingStageEnded {
			return e
		}
	}
	t.Fatalf("no ended event in %v", stages(events))
	return nil
}

func s4aJob(extra map[string]string) *nexus.Job {
	j := c1Job()
	for k, v := range extra {
		j.Payload[k] = v
	}
	return j
}

func TestMeetingProgressStagesInOrder(t *testing.T) {
	workspace := t.TempDir()
	h := NewMeetingJoinHandler(workspace)
	h.diskPressureFn = func(string) bool { return false }
	now := time.Now()
	h.now = func() time.Time { return now }
	browser := &disclosureBrowser{text: meetingpolicy.Announcement("Recorder", "Owner"), confirmed: true, ended: true}
	media := &pipelineMedia{browser: browser, path: filepath.Join(workspace, "meetings", "synthetic.wav")}
	h.newMedia = func(meetingJoinParams) MeetingMedia { return media }
	h.transcribeFinal = func(JobContext, *nexus.Job, string) ([]byte, error) { return []byte(`{"text":"t"}`), nil }
	got, progress := progressRecorder()
	if _, err := h.Execute(JobContext{Progress: progress}, c1Job()); err != nil {
		t.Fatal(err)
	}
	want := []string{MeetingStageJoining, MeetingStageWaitingAdmission, MeetingStageAdmitted, MeetingStageEnded, MeetingStageTranscribing}
	if g := stages(*got); len(g) != len(want) {
		t.Fatalf("stages %v, want %v", g, want)
	} else {
		for i := range want {
			if g[i] != want[i] {
				t.Fatalf("stages %v, want %v", g, want)
			}
		}
	}
	if e := endedReason(t, *got); e["reason"] != "call_ended" || e["notify_owner"] != nil {
		t.Fatalf("ended event %v", e)
	}
}

// A lobby denial is distinct from an admission timeout, hints the owner, and
// an undecided lobby never reads as a denial.
func TestMeetingDenialIsDistinctFromAdmitTimeout(t *testing.T) {
	for _, denied := range []bool{true, false} {
		h := NewMeetingJoinHandler(t.TempDir())
		h.diskPressureFn = func(string) bool { return false }
		browser := &lobbyBrowser{denied: denied}
		h.newMedia = func(meetingJoinParams) MeetingMedia { return &pipelineMedia{browser: browser} }
		got, progress := progressRecorder()
		_, err := h.Execute(JobContext{Progress: progress}, s4aJob(map[string]string{"admit_timeout_seconds": "1"}))
		e := endedReason(t, *got)
		if denied {
			if !errors.Is(err, ErrMeetingDenied) || e["reason"] != MeetingEndDenied || e["notify_owner"] != true {
				t.Fatalf("denial: err=%v ended=%v", err, e)
			}
		} else if !errors.Is(err, errMeetingAdmitTimeout) || errors.Is(err, ErrMeetingDenied) || e["reason"] != MeetingEndAdmitTimeout || e["notify_owner"] != nil {
			t.Fatalf("timeout: err=%v ended=%v", err, e)
		}
	}
}

// A Stop during recording leaves the call, still transcribes, and reports
// "stopped", never "completed".
func TestMeetingStopDuringRecordingLeavesAndTranscribes(t *testing.T) {
	workspace := t.TempDir()
	h := NewMeetingJoinHandler(workspace)
	h.diskPressureFn = func(string) bool { return false }
	stop := false
	browser := &disclosureBrowser{text: meetingpolicy.Announcement("Recorder", "Owner"), confirmed: true, onEnd: func() { stop = true }}
	media := &pipelineMedia{browser: browser, path: filepath.Join(workspace, "meetings", "synthetic.wav")}
	h.newMedia = func(meetingJoinParams) MeetingMedia { return media }
	transcribed := false
	h.transcribeFinal = func(JobContext, *nexus.Job, string) ([]byte, error) {
		transcribed = true
		return []byte(`{"text":"partial"}`), nil
	}
	got, progress := progressRecorder()
	out, err := h.Execute(JobContext{Progress: progress, CancelRequested: func() bool { return stop }}, c1Job())
	var result map[string]any
	if err != nil || json.Unmarshal(out, &result) != nil {
		t.Fatal(err)
	}
	if result["status"] != "stopped" || result["end_reason"] != MeetingEndStoppedByUser || !transcribed || !browser.leave || !media.stopped {
		t.Fatalf("stop result %v transcribed=%v left=%v", result, transcribed, browser.leave)
	}
	if e := endedReason(t, *got); e["reason"] != MeetingEndStoppedByUser {
		t.Fatalf("ended event %v", e)
	}
}

// A Stop before recording is a caller cancellation: nothing recorded.
func TestMeetingStopBeforeAdmissionIsCancellation(t *testing.T) {
	h := NewMeetingJoinHandler(t.TempDir())
	h.diskPressureFn = func(string) bool { return false }
	browser := &lobbyBrowser{}
	media := &pipelineMedia{browser: browser}
	h.newMedia = func(meetingJoinParams) MeetingMedia { return media }
	got, progress := progressRecorder()
	_, err := h.Execute(JobContext{Progress: progress, CancelRequested: func() bool { return true }}, c1Job())
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrMeetingTeardownUnconfirmed) || media.stopped {
		t.Fatalf("pre-recording stop: err=%v recorded=%v", err, media.stopped)
	}
	if e := endedReason(t, *got); e["reason"] != MeetingEndStoppedByUser {
		t.Fatalf("ended event %v", e)
	}
}

func TestMeetingCapabilitiesKeepMeetingBudgetAndLane(t *testing.T) {
	c := NewMeetingJoinHandler("").Capabilities()
	if !c.CooperativeCancel || c.Deadline != DeadlineUnset || c.Lane != LaneNormal || len(c.Stages) != 5 {
		t.Fatalf("capabilities %+v", c)
	}
}
