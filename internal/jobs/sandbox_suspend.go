// internal/jobs/sandbox_suspend.go
package jobs

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// SandboxSuspendHandler pauses a Docker container for sandbox suspension.
type SandboxSuspendHandler struct{}

// sandboxResult is the JSON structure returned for sandbox operations.
type sandboxResult struct {
	SandboxID   string `json:"sandbox_id"`
	ContainerID string `json:"container_id"`
	Action      string `json:"action"`
	Success     bool   `json:"success"`
	Error       string `json:"error,omitempty"`
}

// Execute pauses the Docker container identified by container_id.
//
// Payload fields:
//   - sandbox_id: logical sandbox identifier
//   - container_id: Docker container ID or name to pause
func (h *SandboxSuspendHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	sandboxID := job.Payload["sandbox_id"]
	if sandboxID == "" {
		return nil, fmt.Errorf("job payload missing 'sandbox_id' field")
	}

	containerID := job.Payload["container_id"]
	if containerID == "" {
		return nil, fmt.Errorf("job payload missing 'container_id' field")
	}

	ctx.Log("info", "     - [Job %s] SANDBOX_SUSPEND sandbox=%s container=%s", job.ID, sandboxID, containerID)

	cmd := exec.Command("docker", "pause", containerID)
	output, err := cmd.CombinedOutput()
	if err != nil {
		errMsg := fmt.Sprintf("docker pause failed: %s", strings.TrimSpace(string(output)))
		return json.Marshal(sandboxResult{
			SandboxID:   sandboxID,
			ContainerID: containerID,
			Action:      "suspend",
			Success:     false,
			Error:       errMsg,
		})
	}

	return json.Marshal(sandboxResult{
		SandboxID:   sandboxID,
		ContainerID: containerID,
		Action:      "suspend",
		Success:     true,
	})
}

// Ensure SandboxSuspendHandler implements JobHandler.
var _ JobHandler = (*SandboxSuspendHandler)(nil)
