package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

type servicePolicyRecorder struct {
	calls   int
	payload map[string]string
}

func (f *servicePolicyRecorder) Execute(_ jobs.JobContext, job *nexus.Job) ([]byte, error) {
	f.calls++
	f.payload = job.Payload
	return []byte("service-result"), nil
}

func servicePolicyFixture(t *testing.T, kind string) (*LegacyHandlerAdapter, *servicePolicyRecorder, LegacyHandlerOpts, *Job) {
	t.Helper()
	opts := LegacyHandlerOpts{ServiceOrgID: "example", ServiceNodeID: "node-a", PermissionsDir: t.TempDir()}
	recorder := &servicePolicyRecorder{}
	h := newServiceLegacyHandlerAdapter(kind, recorder, opts)
	job := &Job{ID: "example-job", Type: kind, Source: "redis-api", SourceQueue: "jobs:v1:shell:org_example:node:node-a", Payload: map[string]any{"service": "example-service"}}
	return h, recorder, opts, job
}

func TestServicePolicyAcceptedDispatch(t *testing.T) {
	for _, kind := range []string{JobTypeServiceStart, JobTypeServiceStop, JobTypeServiceStatus} {
		for _, source := range []string{"redis", "redis-api"} {
			t.Run(kind+"/"+source, func(t *testing.T) {
				h, recorder, _, job := servicePolicyFixture(t, kind)
				job.Source = source
				job.Payload["env"] = map[string]any{"EXAMPLE": "value"}
				if !h.CanHandle(kind) {
					t.Fatal("configured service operation not advertised")
				}
				result, err := h.Execute(context.Background(), job, nil)
				if err != nil || result.Status != JobStatusSuccess || recorder.calls != 1 {
					t.Fatalf("dispatch = %+v, %v, calls=%d", result, err, recorder.calls)
				}
				if recorder.payload["env"] != `{"EXAMPLE":"value"}` {
					t.Fatal("structured payload was not preserved")
				}
			})
		}
	}
}

func TestServicePolicyRoutingValidation(t *testing.T) {
	for _, kind := range []string{JobTypeServiceStart, JobTypeServiceStop, JobTypeServiceStatus} {
		for name, mutate := range map[string]func(*Job){
			"unscoped":               func(j *Job) { j.SourceQueue = "jobs:v1:shell:org_example" },
			"different node":         func(j *Job) { j.SourceQueue = "jobs:v1:shell:org_example:node:node-b" },
			"different organization": func(j *Job) { j.SourceQueue = "jobs:v1:shell:org_other:node:node-a" },
			"empty queue":            func(j *Job) { j.SourceQueue = "" },
			"different source":       func(j *Job) { j.Source = "nexus" },
			"payload queue only":     func(j *Job) { j.SourceQueue = ""; j.Payload["source_queue"] = "jobs:v1:shell:org_example:node:node-a" },
			"different target":       func(j *Job) { j.Payload["target_node"] = "node-b" },
			"different identity":     func(j *Job) { j.Payload["node_id"] = "node-b" },
			"non-string identity":    func(j *Job) { j.Payload["node_id"] = 7 },
		} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				h, recorder, _, job := servicePolicyFixture(t, kind)
				mutate(job)
				_, err := h.Execute(context.Background(), job, nil)
				if err == nil || recorder.calls != 0 {
					t.Fatalf("routing validation = %v, calls=%d", err, recorder.calls)
				}
			})
		}
	}
}

func TestServicePolicyLivePermissions(t *testing.T) {
	h, recorder, opts, job := servicePolicyFixture(t, JobTypeServiceStart)
	p := config.DefaultPermissions()
	if !h.CanHandle(job.Type) {
		t.Fatal("initial registration")
	}
	p.Services = false
	if err := config.SavePermissions(opts.PermissionsDir, p); err != nil {
		t.Fatal(err)
	}
	if h.CanHandle(job.Type) {
		t.Fatal("disabled operation advertised")
	}
	if _, err := h.Execute(context.Background(), job, nil); shellRefusalReason(t, err) != "services_disabled" || recorder.calls != 0 {
		t.Fatalf("live services permission: %v", err)
	}
	p.Services = true
	if err := config.SavePermissions(opts.PermissionsDir, p); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Execute(context.Background(), job, nil); err != nil || recorder.calls != 1 {
		t.Fatalf("reenabled: %v", err)
	}
	if err := os.WriteFile(filepath.Join(opts.PermissionsDir, "permissions.yaml"), []byte("services: ["), 0600); err != nil {
		t.Fatal(err)
	}
	if h.CanHandle(job.Type) {
		t.Fatal("invalid configuration advertised")
	}
	if _, err := h.Execute(context.Background(), job, nil); shellRefusalReason(t, err) != "service_policy_unavailable" || recorder.calls != 1 {
		t.Fatalf("invalid configuration: %v", err)
	}
}

func TestServicePolicyHostNetworking(t *testing.T) {
	for _, value := range []any{true, "true", " T ", "1"} {
		t.Run("request", func(t *testing.T) {
			h, recorder, opts, job := servicePolicyFixture(t, JobTypeServiceStart)
			job.Payload["host_network"] = value
			if _, err := h.Execute(context.Background(), job, nil); shellRefusalReason(t, err) != "host_network_disabled" || recorder.calls != 0 {
				t.Fatalf("default permission: %v", err)
			}
			p := config.DefaultPermissions()
			p.HostNetwork = true
			if err := config.SavePermissions(opts.PermissionsDir, p); err != nil {
				t.Fatal(err)
			}
			if _, err := h.Execute(context.Background(), job, nil); err != nil || recorder.calls != 1 {
				t.Fatalf("local opt-in: %v", err)
			}
			p.HostNetwork = false
			if err := config.SavePermissions(opts.PermissionsDir, p); err != nil {
				t.Fatal(err)
			}
			if _, err := h.Execute(context.Background(), job, nil); shellRefusalReason(t, err) != "host_network_disabled" || recorder.calls != 1 {
				t.Fatalf("updated permission: %v", err)
			}
		})
	}
	for _, value := range []any{false, "false", "0", "", nil} {
		h, recorder, _, job := servicePolicyFixture(t, JobTypeServiceStart)
		job.Payload["host_network"] = value
		if _, err := h.Execute(context.Background(), job, nil); err != nil || recorder.calls != 1 {
			t.Fatalf("default networking %v: %v", value, err)
		}
	}
	for _, value := range []any{"maybe", 1, map[string]any{"enabled": true}} {
		h, recorder, _, job := servicePolicyFixture(t, JobTypeServiceStart)
		job.Payload["host_network"] = value
		if _, err := h.Execute(context.Background(), job, nil); shellRefusalReason(t, err) != "invalid_request" || recorder.calls != 0 {
			t.Fatalf("invalid request: %v", err)
		}
	}
}

func TestServicePolicyRequiresLocalIdentity(t *testing.T) {
	for _, opts := range []LegacyHandlerOpts{{}, {ServiceOrgID: "example"}, {ServiceOrgID: "example", ServiceNodeID: "node-a"}, {ServiceOrgID: "bad:org", ServiceNodeID: "node-a", PermissionsDir: t.TempDir()}} {
		recorder := &servicePolicyRecorder{}
		h := newServiceLegacyHandlerAdapter(JobTypeServiceStart, recorder, opts)
		if h.CanHandle(JobTypeServiceStart) {
			t.Fatal("incomplete configuration advertised")
		}
		if _, err := h.Execute(context.Background(), nil, nil); shellRefusalReason(t, err) != "service_policy_unavailable" || recorder.calls != 0 {
			t.Fatalf("configuration: %v", err)
		}
	}
}

func TestServicePolicyRegisteredHandlers(t *testing.T) {
	opts := LegacyHandlerOpts{ConfigDir: t.TempDir(), PermissionsDir: t.TempDir(), ServiceOrgID: "example", ServiceNodeID: "node-a"}
	handlers := CreateLegacyHandlersWithOpts(opts)
	for _, kind := range []string{JobTypeServiceStart, JobTypeServiceStop, JobTypeServiceStatus} {
		found := false
		for _, h := range handlers {
			if !h.CanHandle(kind) {
				continue
			}
			found = true
			_, err := h.Execute(context.Background(), &Job{Type: kind, Source: "redis", SourceQueue: "jobs:v1:shell:org_example"}, nil)
			if shellRefusalReason(t, err) != "service_stream_mismatch" {
				t.Fatalf("registration for %s: %v", kind, err)
			}
		}
		if !found {
			t.Fatalf("missing configured %s", kind)
		}
	}
}
