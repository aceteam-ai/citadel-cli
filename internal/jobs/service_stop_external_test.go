// internal/jobs/service_stop_external_test.go
package jobs

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/services"
)

// stopResultJSON decodes the subset of serviceResult the stop path emits.
type stopResultJSON struct {
	Name    string `json:"name"`
	Running bool   `json:"running"`
	Kind    string `json:"kind"`
	Action  string `json:"action"`
	Error   string `json:"error"`
	Message string `json:"message"`
}

// TestServiceStopNativeExternallyManaged pins the #1144 SERVICE_STOP posture: a
// native engine owned by a host systemd unit is a clear no-op (running:true,
// guidance) -- mirroring #1084's adopted-docker branch -- not a failure and not
// a false stop. Fully hermetic: the native-running check and the stop are seams,
// so no real process is inspected or signalled.
func TestServiceStopNativeExternallyManaged(t *testing.T) {
	origRunning := isNativeServiceRunningFn
	origStop := stopNativeServiceFn
	t.Cleanup(func() {
		isNativeServiceRunningFn = origRunning
		stopNativeServiceFn = origStop
	})

	isNativeServiceRunningFn = func(name string) bool { return true }
	stopCalled := false
	stopNativeServiceFn = func(name string) error {
		stopCalled = true
		return &services.ErrNativeExternallyManaged{Unit: "ollama.service"}
	}

	h := &ServiceHandler{}
	out, err := h.serviceStop(JobContext{}, manifestService{Name: "ollama", Type: "native"})
	if err != nil {
		t.Fatalf("serviceStop returned a Go error: %v", err)
	}
	if !stopCalled {
		t.Fatal("serviceStop did not consult the native-stop seam")
	}

	var res stopResultJSON
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !res.Running {
		t.Error("running = false; an externally-managed engine is still running")
	}
	if res.Kind != "native" {
		t.Errorf("kind = %q, want native", res.Kind)
	}
	if res.Error != "" {
		t.Errorf("error = %q; an externally-managed engine is a no-op, not a failure", res.Error)
	}
	if !strings.Contains(res.Message, "sudo systemctl stop ollama") ||
		!strings.Contains(res.Message, "host systemd unit ollama.service") {
		t.Errorf("message missing guidance: %q", res.Message)
	}
}

// TestServiceStopNativeStartedByCitadel pins that the ordinary (nil-error)
// native stop is unchanged: a real stop reports running:false success.
func TestServiceStopNativeStartedByCitadel(t *testing.T) {
	origRunning := isNativeServiceRunningFn
	origStop := stopNativeServiceFn
	t.Cleanup(func() {
		isNativeServiceRunningFn = origRunning
		stopNativeServiceFn = origStop
	})

	isNativeServiceRunningFn = func(name string) bool { return true }
	stopNativeServiceFn = func(name string) error { return nil }

	h := &ServiceHandler{}
	out, err := h.serviceStop(JobContext{}, manifestService{Name: "ollama", Type: "native"})
	if err != nil {
		t.Fatalf("serviceStop: %v", err)
	}
	var res stopResultJSON
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res.Running {
		t.Error("running = true; a successful stop should report not running")
	}
	if res.Error != "" {
		t.Errorf("unexpected error: %q", res.Error)
	}
}
