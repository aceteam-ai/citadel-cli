// internal/jobs/service_handler_payload_test.go
package jobs

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// newHandlerWithStore returns a ServiceHandler whose instance store is a
// throwaway temp file, so payload STOP/STATUS resolution is tested without
// touching the real ~/.citadel.
func newHandlerWithStore(t *testing.T) *ServiceHandler {
	t.Helper()
	h := NewServiceHandler(t.TempDir())
	h.instances = &instanceStore{path: filepath.Join(t.TempDir(), "instances.json")}
	return h
}

// TestExecute_PayloadStartRejectsDisallowedImage proves the extended-payload
// branch is reached (an image-bearing SERVICE_START does not hit the manifest
// path) and that registry validation runs before any docker call.
func TestExecute_PayloadStartRejectsDisallowedImage(t *testing.T) {
	h := newHandlerWithStore(t)
	job := &nexus.Job{
		ID:   "j1",
		Type: "SERVICE_START",
		Payload: map[string]string{
			"service":           "ac-x",
			"image":             "docker.io/library/nginx", // disallowed registry
			"host_port":         "18789",
			"state_volume_path": "~/citadel-cache/instances/x",
		},
	}
	_, err := h.Execute(JobContext{}, job)
	if err == nil {
		t.Fatal("expected error for disallowed image, got nil")
	}
	if want := "not from an allowed registry"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want substring %q", err.Error(), want)
	}
}

// TestExecute_PayloadStatusFromStore proves SERVICE_STATUS resolves a
// payload-launched instance from the instance store (it is in neither the
// manifest nor the embedded ServiceMap) and returns a status result without
// falling through to the manifest lookup.
func TestExecute_PayloadStatusFromStore(t *testing.T) {
	h := newHandlerWithStore(t)
	if err := h.instances.Put(InstanceRecord{
		ServiceName:   "ac-x",
		ContainerName: "citadel-ac-x",
		Image:         "ghcr.io/aceteam-ai/claudecode-service:latest",
		HostPort:      18789,
		ContainerPort: 8787,
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	job := &nexus.Job{ID: "j2", Type: "SERVICE_STATUS", Payload: map[string]string{"service": "ac-x"}}
	out, err := h.Execute(JobContext{}, job)
	if err != nil {
		t.Fatalf("Execute STATUS: %v", err)
	}
	var res serviceResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res.Name != "ac-x" || res.Action != "status" || res.Kind != "docker" {
		t.Errorf("unexpected status result: %+v", res)
	}
	// docker is not present in the unit-test env, so the container is not
	// running -- the point is that STATUS resolved via the store and returned a
	// well-formed result rather than "not found in manifest".
	if res.Running {
		t.Errorf("expected Running=false in unit env, got true")
	}
}
