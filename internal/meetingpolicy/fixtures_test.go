package meetingpolicy

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestMeetingPairedNumericFixtureMatchesRatifiedProfile(t *testing.T) {
	data, err := os.ReadFile("../../docs/meeting-pipeline-budget-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Phases map[string]int64 `json:"phases_seconds"`
		Cases  []struct {
			Name      string `json:"name"`
			Admission int64  `json:"admission_seconds"`
			Recording int64  `json:"recording_seconds"`
			Execution int64  `json:"execution_seconds"`
			Stuck     int64  `json:"stuck_after_seconds"`
			Wait      int64  `json:"producer_wait_seconds"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	p := Default()
	phases := map[string]int64{"setup": p.Setup, "admission_default": p.AdmissionDefault, "admission_max": p.AdmissionMax, "recording_default": p.RecordingDefault, "recording_min": MinRecordingSeconds, "recording_max": p.RecordingMax, "transcription": p.Transcription, "cleanup_total": p.Cleanup, "preclaim_queue": p.Queue, "claim_handoff": p.Handoff, "stuck_recovery": p.Recovery, "final_receipt": p.Receipt}
	if len(phases) != len(fixture.Phases) || len(fixture.Cases) != 3 {
		t.Fatal("paired fixture shape changed")
	}
	for k, v := range phases {
		if fixture.Phases[k] != v {
			t.Fatal("paired phase mismatch", k)
		}
	}
	for _, c := range fixture.Cases {
		b, e := p.Budget(strconv.FormatInt(c.Admission, 10), true, strconv.FormatInt(c.Recording, 10), true)
		if e != nil || b.Execution() != time.Duration(c.Execution)*time.Second || b.StuckAfter() != time.Duration(c.Stuck)*time.Second || b.ProducerWait() != time.Duration(c.Wait)*time.Second {
			t.Fatal("paired budget mismatch", c.Name, e)
		}
	}
}
