package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/containerjobs"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

const containerFixtureOrg = "00000000-0000-4000-8000-000000000001"
const containerForeignOrg = "00000000-0000-4000-8000-000000000002"

type containerAdmissionProbe struct {
	containerjobs.Runtime
	locks   int
	failure error
}

func (p *containerAdmissionProbe) Lock(context.Context) (func(), error) {
	p.locks++
	return nil, p.failure
}

func containerAuthorityFixture(org string, p *containerAdmissionProbe) *ContainerJobHandler {
	return &ContainerJobHandler{kind: containerjobs.Stop, enabled: func() bool { return true }, engine: &containerjobs.Engine{Runtime: p, EnrolledOrg: func() string { return org }}}
}
func containerStopJob(org, queue string) *Job {
	return &Job{Type: containerjobs.Stop, SourceQueue: queue, RawPayload: []byte(`{"org_id":"` + org + `","container_ref":"sandbox-1234abcd"}`), Payload: map[string]any{"org_id": containerFixtureOrg, "container_ref": "sandbox-1234abcd"}}
}
func TestContainerAdmissionRequiresPerNodeQueueAndOriginalBytes(t *testing.T) {
	for _, queue := range []string{"", "jobs:v1:gpu-general", "jobs:v1:tag:cpu:org_x", "jobs:v1:shell:org_x"} {
		p := &containerAdmissionProbe{failure: containerjobs.ErrUnavailable}
		h := containerAuthorityFixture(containerFixtureOrg, p)
		if _, err := h.Execute(context.Background(), containerStopJob(containerFixtureOrg, queue), nil); err == nil || p.locks != 0 {
			t.Fatal("shared queue touched runtime")
		}
	}
	for _, raw := range [][]byte{nil, []byte("invalid")} {
		p := &containerAdmissionProbe{failure: containerjobs.ErrUnavailable}
		h := containerAuthorityFixture(containerFixtureOrg, p)
		j := containerStopJob(containerFixtureOrg, "jobs:v1:shell:org_x:node:42")
		j.RawPayload = raw
		if _, err := h.Execute(context.Background(), j, nil); err == nil || p.locks != 0 {
			t.Fatal("diagnostic map substituted execution bytes")
		}
	}
	p := &containerAdmissionProbe{failure: containerjobs.ErrUnavailable}
	h := containerAuthorityFixture(containerFixtureOrg, p)
	_, _ = h.Execute(context.Background(), containerStopJob(containerFixtureOrg, "jobs:v1:shell:org_x:node:42"), nil)
	if p.locks != 1 {
		t.Fatal("valid exact-node request never reached runtime boundary")
	}
}
func TestContainerNodeOrgBindingAndNoOwnershipOracle(t *testing.T) {
	p := &containerAdmissionProbe{failure: containerjobs.ErrUnavailable}
	h := containerAuthorityFixture(containerFixtureOrg, p)
	_, foreign := h.Execute(context.Background(), containerStopJob(containerForeignOrg, "jobs:v1:shell:org_x:node:42"), nil)
	if foreign == nil || p.locks != 0 {
		t.Fatal("foreign raw org reached runtime")
	}
	var first *jobs.ShellRefusal
	if !errors.As(foreign, &first) {
		t.Fatal("not a terminal refusal")
	}
	for _, cause := range []error{containerjobs.ErrOwnership, containerjobs.ErrNotFound, containerjobs.ErrUnavailable} {
		p.failure = cause
		_, err := h.Execute(context.Background(), containerStopJob(containerFixtureOrg, "jobs:v1:shell:org_x:node:42"), nil)
		var refusal *jobs.ShellRefusal
		if !errors.As(err, &refusal) || refusal.Reason != first.Reason || refusal.Message != first.Message {
			t.Fatal("ownership differs from missing/unavailable")
		}
	}
}
