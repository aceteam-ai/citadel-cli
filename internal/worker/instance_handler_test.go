package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/proxmox"
)

// fakeInstanceProvider records calls and returns canned results.
type fakeInstanceProvider struct {
	calls     []string
	failWith  error
	statusRes *proxmox.InstanceStatus
}

func (f *fakeInstanceProvider) Provision(ctx context.Context, req proxmox.ProvisionRequest) (*proxmox.ProvisionResult, error) {
	f.calls = append(f.calls, "provision:"+req.InstanceID)
	if f.failWith != nil {
		return nil, f.failWith
	}
	return &proxmox.ProvisionResult{VMID: 105, PVENode: "pve1", Name: "test", Cores: 2, MemoryMB: 4096, DiskGB: 40, Pool: "aceteam-org-x"}, nil
}

func (f *fakeInstanceProvider) Start(ctx context.Context, ref proxmox.InstanceRef) error {
	f.calls = append(f.calls, fmt.Sprintf("start:%d", ref.VMID))
	return f.failWith
}

func (f *fakeInstanceProvider) Stop(ctx context.Context, ref proxmox.InstanceRef) error {
	f.calls = append(f.calls, fmt.Sprintf("stop:%d", ref.VMID))
	return f.failWith
}

func (f *fakeInstanceProvider) Destroy(ctx context.Context, ref proxmox.InstanceRef, instanceID string) error {
	f.calls = append(f.calls, fmt.Sprintf("destroy:%d:%s", ref.VMID, instanceID))
	return f.failWith
}

func (f *fakeInstanceProvider) Status(ctx context.Context, ref proxmox.InstanceRef) (*proxmox.InstanceStatus, error) {
	f.calls = append(f.calls, fmt.Sprintf("status:%d", ref.VMID))
	if f.failWith != nil {
		return nil, f.failWith
	}
	if f.statusRes != nil {
		return f.statusRes, nil
	}
	return &proxmox.InstanceStatus{VMID: ref.VMID, Status: "running", UptimeSeconds: 42}, nil
}

// perNodeQueue is declared in agent_update_test.go.
const (
	hypervisorQueue = "jobs:v1:tag:hypervisor:proxmox"
	orgPoolQueue    = "jobs:v1:shell:org_abc"
)

func newTestInstanceHandler(fake *fakeInstanceProvider, providerErr error) *InstanceHandler {
	return NewInstanceHandler(InstanceHandlerConfig{
		Provider: func() (InstanceProvider, error) {
			if providerErr != nil {
				return nil, providerErr
			}
			return fake, nil
		},
	})
}

func instanceJob(jobType, queue string, payload map[string]any) *Job {
	return &Job{ID: "job-1", Type: jobType, Payload: payload, SourceQueue: queue}
}

func TestInstanceHandlerCanHandle(t *testing.T) {
	h := newTestInstanceHandler(&fakeInstanceProvider{}, nil)
	for _, jt := range []string{
		JobTypeInstanceProvision, JobTypeInstanceStart, JobTypeInstanceStop,
		JobTypeInstanceDestroy, JobTypeInstanceStatus,
	} {
		if !h.CanHandle(jt) {
			t.Errorf("expected CanHandle(%s)", jt)
		}
	}
	if h.CanHandle(JobTypeShellCommand) || h.CanHandle(JobTypeInstanceMessage) {
		t.Error("must not handle unrelated job types (INSTANCE_MESSAGE is the agent-instance family)")
	}
}

func TestInstanceHandlerRefusesOrgPoolQueue(t *testing.T) {
	fake := &fakeInstanceProvider{}
	h := newTestInstanceHandler(fake, nil)
	res, err := h.Execute(context.Background(), instanceJob(JobTypeInstanceProvision, orgPoolQueue, map[string]any{
		"instance_id": "i-1",
	}), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != JobStatusFailure {
		t.Fatalf("expected failure, got %s", res.Status)
	}
	if len(fake.calls) != 0 {
		t.Error("provider must not be invoked for a refused queue")
	}
}

func TestInstanceHandlerAcceptsHypervisorAndPerNodeQueues(t *testing.T) {
	for _, q := range []string{hypervisorQueue, hypervisorQueue + "_dev", perNodeQueue} {
		fake := &fakeInstanceProvider{}
		h := newTestInstanceHandler(fake, nil)
		res, err := h.Execute(context.Background(), instanceJob(JobTypeInstanceStatus, q, map[string]any{
			"instance_id": "i-1", "vmid": 105,
		}), &NoOpStreamWriter{})
		if err != nil {
			t.Fatalf("Execute(%s): %v", q, err)
		}
		if res.Status != JobStatusSuccess {
			t.Fatalf("queue %s: expected success, got %s (%v)", q, res.Status, res.Error)
		}
	}
}

func TestInstanceHandlerProvision(t *testing.T) {
	fake := &fakeInstanceProvider{}
	h := newTestInstanceHandler(fake, nil)
	res, err := h.Execute(context.Background(), instanceJob(JobTypeInstanceProvision, hypervisorQueue, map[string]any{
		"instance_id":   "i-1",
		"name":          "box",
		"instance_type": "medium",
		"org_id":        "org-1",
		"authkey":       "hskey-x",
	}), &NoOpStreamWriter{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != JobStatusSuccess {
		t.Fatalf("expected success, got %s: %v", res.Status, res.Error)
	}
	if res.Output["vmid"] != 105 || res.Output["pve_node"] != "pve1" {
		t.Errorf("unexpected output: %v", res.Output)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "provision:i-1" {
		t.Errorf("unexpected calls: %v", fake.calls)
	}
}

func TestInstanceHandlerProvisionDedupesRedelivery(t *testing.T) {
	// The runner Nacks failed jobs, so a failed provision is redelivered on the
	// same per-node stream. A second attempt must be refused without touching
	// the provider (it could clone a second VM the platform never hears about).
	fake := &fakeInstanceProvider{failWith: errors.New("resize failed")}
	h := newTestInstanceHandler(fake, nil)
	payload := map[string]any{
		"instance_id": "i-dup", "instance_type": "small", "org_id": "o", "authkey": "k",
	}

	res, _ := h.Execute(context.Background(), instanceJob(JobTypeInstanceProvision, hypervisorQueue, payload), &NoOpStreamWriter{})
	if res.Status != JobStatusFailure {
		t.Fatalf("expected first attempt to fail, got %s", res.Status)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("expected one provider call, got %v", fake.calls)
	}

	res, _ = h.Execute(context.Background(), instanceJob(JobTypeInstanceProvision, hypervisorQueue, payload), &NoOpStreamWriter{})
	if res.Status != JobStatusFailure {
		t.Fatalf("expected redelivery to fail terminally, got %s", res.Status)
	}
	if len(fake.calls) != 1 {
		t.Errorf("provider must not run again on redelivery: %v", fake.calls)
	}
	if !strings.Contains(res.Error.Error(), "already attempted") {
		t.Errorf("expected already-attempted error, got %v", res.Error)
	}
}

func TestInstanceHandlerProvisionFailureIsTerminal(t *testing.T) {
	fake := &fakeInstanceProvider{failWith: errors.New("org at cap")}
	h := newTestInstanceHandler(fake, nil)
	res, _ := h.Execute(context.Background(), instanceJob(JobTypeInstanceProvision, hypervisorQueue, map[string]any{
		"instance_id": "i-1", "instance_type": "small", "org_id": "o", "authkey": "k",
	}), &NoOpStreamWriter{})
	// Provisioning is not idempotent: never retry (a retry could double-clone).
	if res.Status != JobStatusFailure {
		t.Fatalf("expected terminal failure, got %s", res.Status)
	}
}

func TestInstanceHandlerLifecycleOps(t *testing.T) {
	cases := []struct {
		jobType string
		want    string
	}{
		{JobTypeInstanceStart, "start:105"},
		{JobTypeInstanceStop, "stop:105"},
		{JobTypeInstanceDestroy, "destroy:105:i-1"},
		{JobTypeInstanceStatus, "status:105"},
	}
	for _, tc := range cases {
		t.Run(tc.jobType, func(t *testing.T) {
			fake := &fakeInstanceProvider{}
			h := newTestInstanceHandler(fake, nil)
			res, err := h.Execute(context.Background(), instanceJob(tc.jobType, perNodeQueue, map[string]any{
				"instance_id": "i-1", "vmid": 105, "pve_node": "pve1",
			}), &NoOpStreamWriter{})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if res.Status != JobStatusSuccess {
				t.Fatalf("expected success, got %s: %v", res.Status, res.Error)
			}
			if len(fake.calls) != 1 || fake.calls[0] != tc.want {
				t.Errorf("unexpected calls: %v (want %s)", fake.calls, tc.want)
			}
		})
	}
}

func TestInstanceHandlerLifecycleRequiresVMID(t *testing.T) {
	h := newTestInstanceHandler(&fakeInstanceProvider{}, nil)
	res, _ := h.Execute(context.Background(), instanceJob(JobTypeInstanceStop, perNodeQueue, map[string]any{
		"instance_id": "i-1",
	}), &NoOpStreamWriter{})
	if res.Status != JobStatusFailure {
		t.Fatalf("expected failure without vmid, got %s", res.Status)
	}
}

func TestInstanceHandlerLifecycleFailureRetries(t *testing.T) {
	fake := &fakeInstanceProvider{failWith: errors.New("connection refused")}
	h := newTestInstanceHandler(fake, nil)
	res, _ := h.Execute(context.Background(), instanceJob(JobTypeInstanceStop, perNodeQueue, map[string]any{
		"instance_id": "i-1", "vmid": 105,
	}), &NoOpStreamWriter{})
	if res.Status != JobStatusRetry {
		t.Fatalf("expected retry on transient lifecycle failure, got %s", res.Status)
	}
}

func TestInstanceHandlerUnconfiguredProviderFailsClearly(t *testing.T) {
	h := newTestInstanceHandler(nil, errors.New("proxmox is not configured on this node"))
	res, _ := h.Execute(context.Background(), instanceJob(JobTypeInstanceStatus, hypervisorQueue, map[string]any{
		"instance_id": "i-1", "vmid": 105,
	}), &NoOpStreamWriter{})
	if res.Status != JobStatusFailure {
		t.Fatalf("expected failure, got %s", res.Status)
	}
	if res.Error == nil || res.Output["error"] == "" {
		t.Error("expected a clear error message")
	}
}

func TestInstanceHandlerStatusPayloadUsesNumericVMID(t *testing.T) {
	// Redis payloads decode numbers as float64; ensure the typed decode copes.
	fake := &fakeInstanceProvider{}
	h := newTestInstanceHandler(fake, nil)
	res, _ := h.Execute(context.Background(), instanceJob(JobTypeInstanceStatus, hypervisorQueue, map[string]any{
		"instance_id": "i-1", "vmid": float64(105),
	}), &NoOpStreamWriter{})
	if res.Status != JobStatusSuccess {
		t.Fatalf("expected success, got %s: %v", res.Status, res.Error)
	}
	if fake.calls[0] != "status:105" {
		t.Errorf("vmid not decoded from float64: %v", fake.calls)
	}
}
