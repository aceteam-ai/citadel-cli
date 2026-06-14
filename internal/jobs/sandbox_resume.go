// internal/jobs/sandbox_resume.go
package jobs

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// SandboxResumeHandler unpauses a Docker container for sandbox resumption.
type SandboxResumeHandler struct{}

// Execute unpauses the Docker container identified by container_id.
//
// Payload fields:
//   - sandbox_id: logical sandbox identifier
//   - container_id: Docker container ID or name to unpause
func (h *SandboxResumeHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	sandboxID := job.Payload["sandbox_id"]
	if sandboxID == "" {
		return nil, fmt.Errorf("job payload missing 'sandbox_id' field")
	}

	containerID := job.Payload["container_id"]
	if containerID == "" {
		return nil, fmt.Errorf("job payload missing 'container_id' field")
	}

	ctx.Log("info", "     - [Job %s] SANDBOX_RESUME sandbox=%s container=%s", job.ID, sandboxID, containerID)

	cmd := exec.Command("docker", "unpause", containerID)
	output, err := cmd.CombinedOutput()
	if err != nil {
		errMsg := fmt.Sprintf("docker unpause failed: %s", strings.TrimSpace(string(output)))
		return json.Marshal(sandboxResult{
			SandboxID:   sandboxID,
			ContainerID: containerID,
			Action:      "resume",
			Success:     false,
			Error:       errMsg,
		})
	}

	return json.Marshal(sandboxResult{
		SandboxID:   sandboxID,
		ContainerID: containerID,
		Action:      "resume",
		Success:     true,
	})
}

// Ensure SandboxResumeHandler implements JobHandler.
var _ JobHandler = (*SandboxResumeHandler)(nil)
