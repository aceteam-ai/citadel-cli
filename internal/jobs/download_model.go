// internal/jobs/download_model.go
package jobs

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

type DownloadModelHandler struct{}

func (h *DownloadModelHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	repoURL, repoOk := job.Payload["repo_url"]
	fileName, fileOk := job.Payload["file_name"]
	modelType, typeOk := job.Payload["model_type"]
	if !repoOk || !fileOk || !typeOk {
		return nil, fmt.Errorf("job payload missing 'repo_url', 'file_name', or 'model_type'")
	}

	fullURL, err := url.JoinPath(repoURL, "resolve/main", fileName)
	if err != nil {
		return nil, fmt.Errorf("could not construct valid download URL: %w", err)
	}

	homeDir, _ := os.UserHomeDir()
	destDir := filepath.Join(homeDir, "citadel-cache", modelType)
	destPath := filepath.Join(destDir, fileName)

	ctx.Log("info", "     - [Job %s] Preparing to download model to %s", job.ID, destPath)

	if err := os.MkdirAll(destDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create destination directory %s: %w", destDir, err)
	}

	if _, statErr := os.Stat(destPath); statErr == nil {
		ctx.Log("info", "     - [Job %s] Model already exists. Skipping download.", job.ID)
		return []byte(fmt.Sprintf("Model '%s' already exists at %s", fileName, destPath)), nil
	}

	ctx.Log("info", "     - [Job %s] Starting download...", job.ID)
	cmd := exec.Command("curl", "-L", "--create-dirs", "-o", destPath, fullURL)
	return cmd.CombinedOutput()
}
