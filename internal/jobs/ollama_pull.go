// internal/jobs/ollama_pull.go
package jobs

import (
	"fmt"
	"os/exec"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

type OllamaPullHandler struct{}

func (h *OllamaPullHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	model, modelOk := job.Payload["model"]
	if !modelOk {
		return nil, fmt.Errorf("job payload missing 'model' field")
	}
	ctx.Log("info", "     - [Job %s] Pulling Ollama model '%s'", job.ID, model)
	cmd := exec.Command("docker", "exec", "citadel-ollama", "ollama", "pull", model)
	return cmd.CombinedOutput()
}
