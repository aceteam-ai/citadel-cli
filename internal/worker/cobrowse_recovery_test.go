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
	job := &Job{ID: "job", Type: JobTypeCobrowseSession, SourceQueue: "jobs:v1:shell:org_owner:node:4242", Payload: map[string]any{"SourceQueue": "forged"}}
	if _, err := a.Execute(context.Background(), job, nil); err != nil {
		t.Fatal(err)
	}
	if h.queue != job.SourceQueue {
		t.Fatal("transport evidence lost or replaced by payload")
	}
}
func TestCobrowseRecoveryCapabilityFollowsConfiguredHandler(t *testing.T) {
	enabled := true
	a := newGatedLegacyHandlerAdapter(JobTypeCobrowseSession, jobs.NewCobrowseSessionHandlerForNode("4242", "node-owner"), func() bool { return enabled })
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

// The registered session handler keeps both the live Files gate and the
// recovery node binding; neither wiring replaces the other.
func TestCobrowseSessionHandlerForKeepsFilesGateAndRecoveryBinding(t *testing.T) {
	on := true
	h := cobrowseSessionHandlerFor(LegacyHandlerOpts{CobrowseRecoveryNodeID: "node-fixture", CobrowseRecoveryOrgID: "org-fixture", FilesEnabled: func() bool { return on }})
	if h.Enabled == nil || !h.Enabled() {
		t.Fatal("Files gate not wired")
	}
	if h.CobrowseJobRecoveryAvailable() != cobrowserecovery.Supported() {
		t.Fatal("recovery binding lost")
	}
	if cobrowseSessionHandlerFor(LegacyHandlerOpts{FilesEnabled: func() bool { return on }}).CobrowseJobRecoveryAvailable() {
		t.Fatal("unbound node advertised recovery")
	}
	if cobrowseSessionHandlerFor(LegacyHandlerOpts{CobrowseRecoveryNodeID: "node-fixture"}).CobrowseJobRecoveryAvailable() {
		t.Fatal("node without a stream org advertised recovery")
	}
}
