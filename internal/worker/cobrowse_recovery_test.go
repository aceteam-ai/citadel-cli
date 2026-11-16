package worker

import (
	"context"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowserecovery"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

type fixtureQueueHandler struct{ queue string }

func (f *fixtureQueueHandler) Execute(_ jobs.JobContext, j *nexus.Job) ([]byte, error) {
	f.queue = j.SourceQueue
	return []byte(`{}`), nil
}
func TestCobrowseRecoveryTransportQueueEvidence(t *testing.T) {
	h := &fixtureQueueHandler{}
	a := NewLegacyHandlerAdapter(JobTypeCobrowseSession, h)
	job := &Job{ID: "job", Type: JobTypeCobrowseSession, SourceQueue: "jobs:v1:shell:org_owner:node:1297", Payload: map[string]any{"SourceQueue": "forged"}}
	if _, err := a.Execute(context.Background(), job, nil); err != nil {
		t.Fatal(err)
	}
	if h.queue != job.SourceQueue {
		t.Fatal("transport evidence lost or replaced by payload")
	}
}
func TestCobrowseRecoveryCapabilityFollowsConfiguredHandler(t *testing.T) {
	enabled := true
	a := newGatedLegacyHandlerAdapter(JobTypeCobrowseSession, jobs.NewCobrowseSessionHandlerForNode("1297"), func() bool { return enabled })
	r := &Runner{handlers: []JobHandler{a}}
	if r.CobrowseJobRecoveryAvailable() != cobrowserecovery.Supported() {
		t.Fatal("configured adapter not advertised")
	}
	enabled = false
	if r.CobrowseJobRecoveryAvailable() {
		t.Fatal("disabled adapter advertised")
	}
	r.handlers = []JobHandler{NewLegacyHandlerAdapter(JobTypeCobrowseSession, jobs.NewCobrowseSessionHandler())}
	if r.CobrowseJobRecoveryAvailable() {
		t.Fatal("unbound node advertised")
	}
	r.handlers = nil
	if r.CobrowseJobRecoveryAvailable() {
		t.Fatal("missing adapter advertised")
	}
}
