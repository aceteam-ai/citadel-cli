// internal/jobs/download_model.go
package jobs

import (
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// errInvalidModelRepoURL is returned when repo_url is not an http(s) URL with
// a host. The message is fixed and never echoes the payload value.
var errInvalidModelRepoURL = errors.New("refusing model download: repo_url must be an http or https URL with a host")

// downloadModelCacheRoot returns the directory DOWNLOAD_MODEL writes under.
// A package var so tests can point it at a temp dir.
var downloadModelCacheRoot = func() string {
	home, err := platform.UserHomeDir()
	if err != nil || home == "" {
		return "citadel-cache"
	}
	return filepath.Join(home, "citadel-cache")
}

type DownloadModelHandler struct{}

func (h *DownloadModelHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	repoURL, repoOk := job.Payload["repo_url"]
	fileName, fileOk := job.Payload["file_name"]
	modelType, typeOk := job.Payload["model_type"]
	if !repoOk || !fileOk || !typeOk {
		return nil, fmt.Errorf("job payload missing 'repo_url', 'file_name', or 'model_type'")
	}

	// Validate the path components before they reach the URL or the
	// filesystem.
	if !validModelPathComponent(modelType) || !validModelPathComponent(fileName) {
		return nil, ErrUnsafeModelPath
	}
	if u, err := url.Parse(repoURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errInvalidModelRepoURL
	}

	fullURL, err := url.JoinPath(repoURL, "resolve/main", fileName)
	if err != nil {
		return nil, fmt.Errorf("could not construct valid download URL: %w", err)
	}

	_, destPath, exists, err := resolveModelCacheDest(downloadModelCacheRoot(), modelType, fileName)
	if err != nil {
		return nil, err
	}

	ctx.Log("info", "     - [Job %s] Preparing to download model to %s", job.ID, destPath)

	if exists {
		ctx.Log("info", "     - [Job %s] Model already exists. Skipping download.", job.ID)
		return []byte(fmt.Sprintf("Model '%s' already exists at %s", fileName, destPath)), nil
	}

	ctx.Log("info", "     - [Job %s] Starting download...", job.ID)
	cmd := exec.Command("curl", "-L", "-o", destPath, "--", fullURL)
	return cmd.CombinedOutput()
}
