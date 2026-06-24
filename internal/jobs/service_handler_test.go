// internal/jobs/service_handler_test.go
package jobs

import (
	"strings"
	"testing"
)

// TestComposeEnv_InjectsWorkspace verifies that SERVICE_START exports an
// absolute CITADEL_WORKSPACE to docker compose. The transcribe sidecar compose
// uses ${CITADEL_WORKSPACE:?...}; without this injection a worker started via
// --workspace (or the default path) would have no CITADEL_WORKSPACE in its env
// and compose would fail, leaving the node-local STT path dead.
func TestComposeEnv_InjectsWorkspace(t *testing.T) {
	h := NewServiceHandlerWithWorkspace("/etc/citadel", "/home/u/citadel-node/workspace")
	env := h.composeEnv()

	var got string
	found := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "CITADEL_WORKSPACE=") {
			got = strings.TrimPrefix(kv, "CITADEL_WORKSPACE=")
			found = true
		}
	}
	if !found {
		t.Fatal("composeEnv did not set CITADEL_WORKSPACE")
	}
	if got != "/home/u/citadel-node/workspace" {
		t.Errorf("CITADEL_WORKSPACE = %q, want the workspace dir", got)
	}
}

// TestComposeEnv_NoWorkspaceLeavesEnvUntouched verifies that when no workspace
// is configured we do not inject an empty CITADEL_WORKSPACE (which would mount
// the wrong path); compose's :? guard should then fail loudly instead.
func TestComposeEnv_NoWorkspaceLeavesEnvUntouched(t *testing.T) {
	h := NewServiceHandler("/etc/citadel")
	for _, kv := range h.composeEnv() {
		if strings.HasPrefix(kv, "CITADEL_WORKSPACE=") {
			// Only acceptable if it was already present in the process env.
			if strings.TrimPrefix(kv, "CITADEL_WORKSPACE=") == "" {
				t.Errorf("injected empty CITADEL_WORKSPACE when workspace unset")
			}
		}
	}
}
