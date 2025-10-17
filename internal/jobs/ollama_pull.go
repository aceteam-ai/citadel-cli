package jobs

import (
	"fmt"
	"os/exec"

	"github.com/aceboss/citadel-cli/internal/nexus"
)

type OllamaPullHandler struct{}

func (h *OllamaPullHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	model, modelOk := job.Payload["model"]
	if !modelOk {
		return nil, fmt.Errorf("job payload missing 'model' field")
	}
	fmt.Printf("     - [Job %s] Pulling Ollama model '%s'\n", job.ID, model)
	cmd := exec.Command("docker", "exec", "citadel-ollama", "ollama", "pull", model)
	return cmd.CombinedOutput()
}
