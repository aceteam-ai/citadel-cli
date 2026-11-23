package worker

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestSetupPostureRefusedJobTypes pins the job types a setup-page PC refuses.
func TestSetupPostureRefusedJobTypes(t *testing.T) {
	var got []string
	for k := range setupPostureRefusedJobTypes {
		got = append(got, k)
	}
	sort.Strings(got)
	want := []string{
		JobTypeAndroidBuild, JobTypeAppDeploy, JobTypeAppDestroy, JobTypeAppStart, JobTypeAppStop,
		JobTypeExposeSet, JobTypeGomobileBuild, JobTypeInstanceDestroy, JobTypeInstanceProvision,
		JobTypeInstanceStart, JobTypeInstanceStop, JobTypeModuleSet, JobTypeServiceStart,
		JobTypeServiceStop, JobTypeWhatsAppProvision,
	}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("refused set %v, want %v", got, want)
	}
}

// recordingHandler counts executions of every job type it is offered.
type recordingHandler struct{ runs int }

func (h *recordingHandler) CanHandle(string) bool { return true }
func (h *recordingHandler) Execute(context.Context, *Job, StreamWriter) (*JobResult, error) {
	h.runs++
	return &JobResult{Status: JobStatusSuccess}, nil
}

// TestSetupPostureRefusesTerminally: with the posture on, a gated job never
// reaches its handler and fails once, terminally, with the typed reason; an
// ungated job and a node without the posture run as before.
func TestSetupPostureRefusesTerminally(t *testing.T) {
	on := true
	SetSetupPostureSource(func() bool { return on })
	t.Cleanup(func() { SetSetupPostureSource(nil) })

	run := func(jobType string) (*recordingHandler, *MockJobSource, *MockStreamWriter, bool) {
		h := &recordingHandler{}
		source := NewMockJobSource("test", nil)
		runner := NewRunner(source, []JobHandler{h}, RunnerConfig{WorkerID: "test-worker"})
		stream := &MockStreamWriter{}
		ok := runner.executeJob(context.Background(), &Job{ID: "j-" + jobType, Type: jobType, Metadata: JobMetadata{Attempts: 1, MaxAttempts: 3}}, stream, time.Now(), false, 0)
		return h, source, stream, ok
	}

	for _, jt := range []string{JobTypeServiceStart, JobTypeAppDeploy, JobTypeInstanceProvision, JobTypeModuleSet} {
		h, source, stream, ok := run(jt)
		if ok || h.runs != 0 {
			t.Fatalf("%s ran on a posture PC: ok=%v runs=%d", jt, ok, h.runs)
		}
		if stream.errorCount != 1 || stream.erroredRecover || len(source.FailedJobs()) != 1 || len(source.NackedJobs()) != 0 {
			t.Fatalf("%s: want one terminal error and a Fail: errors=%d failed=%d nacked=%d", jt, stream.errorCount, len(source.FailedJobs()), len(source.NackedJobs()))
		}
		var refusal struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(stream.erroredErr.Error()), &refusal); err != nil || refusal.Reason != ReasonSetupPosture {
			t.Fatalf("%s: refusal reason %q (%v)", jt, refusal.Reason, err)
		}
	}
	if h, _, _, ok := run(JobTypeCobrowseSession); !ok || h.runs != 1 {
		t.Fatalf("co-browse must still run on a posture PC: ok=%v runs=%d", ok, h.runs)
	}
	on = false
	if h, _, _, ok := run(JobTypeServiceStart); !ok || h.runs != 1 {
		t.Fatalf("a node without the posture must run SERVICE_START: ok=%v runs=%d", ok, h.runs)
	}
}
