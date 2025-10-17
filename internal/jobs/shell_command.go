// internal/jobs/shell_command.go
package jobs

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/aceboss/citadel-cli/internal/nexus"
)

type ShellCommandHandler struct{}

func (h *ShellCommandHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	cmdString, ok := job.Payload["command"]
	if !ok {
		return nil, fmt.Errorf("job payload missing 'command' field")
	}
	fmt.Printf("     - [Job %s] Running shell command: '%s'\n", job.ID, cmdString)
	parts := strings.Fields(cmdString)
	cmd := exec.Command(parts[0], parts[1:]...)
	return cmd.CombinedOutput()
}
