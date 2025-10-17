// internal/jobs/download_model.go
package jobs

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/aceboss/citadel-cli/internal/nexus"
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

	fmt.Printf("     - [Job %s] Preparing to download model to %s\n", job.ID, destPath)

	if err := os.MkdirAll(destDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create destination directory %s: %w", destDir, err)
	}

	if _, statErr := os.Stat(destPath); statErr == nil {
		fmt.Printf("     - [Job %s] Model already exists. Skipping download.\n", job.ID)
		return []byte(fmt.Sprintf("Model '%s' already exists at %s", fileName, destPath)), nil
	}

	fmt.Printf("     - [Job %s] Starting download...\n", job.ID)
	cmd := exec.Command("curl", "-L", "--create-dirs", "-o", destPath, fullURL)
	return cmd.CombinedOutput()
}
