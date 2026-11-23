// internal/jobs/download_model.go
package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// errInvalidModelRepoURL is returned when repo_url is not an http(s) URL with
// a host. The message is fixed and never echoes the payload value.
var errInvalidModelRepoURL = errors.New("refusing model download: repo_url must be an http or https URL with a host")

// errModelFileCreate is returned when the destination file cannot be created
// inside the cache. Fixed message, no path.
var errModelFileCreate = errors.New("failed to create the model file in the cache")

// downloadModelCacheRoot returns the directory DOWNLOAD_MODEL writes under.
// A package var so tests can point it at a temp dir.
var downloadModelCacheRoot = func() string {
	home, err := platform.UserHomeDir()
	if err != nil || home == "" {
		return "citadel-cache"
	}
	return filepath.Join(home, "citadel-cache")
}

// downloadModelBeforeOpen runs between path validation and opening the
// destination file. A no-op in production; tests use it to change the
// filesystem inside that window.
var downloadModelBeforeOpen = func() {}

// runModelDownload runs curl with args, streaming the response body to out.
// It returns curl's stderr. A package var so tests never touch the network.
var runModelDownload = func(ctx context.Context, args []string, out io.Writer) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "curl", args...)
	var stderr bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stderr.Bytes(), err
}

// modelDownloadCurlArgs is the curl argv for fetching fullURL to stdout. Only
// http(s) is allowed, including on redirects, and an HTTP error status fails
// the download instead of saving the error body.
func modelDownloadCurlArgs(fullURL string) []string {
	return []string{
		"--fail", "--silent", "--show-error", "--location",
		"--proto", "=https,http", "--proto-redir", "=https,http",
		"--", fullURL,
	}
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

	destDir, destPath, exists, err := resolveModelCacheDest(downloadModelCacheRoot(), modelType, fileName)
	if err != nil {
		return nil, err
	}

	ctx.Log("info", "     - [Job %s] Preparing to download model to %s", job.ID, destPath)

	if exists {
		ctx.Log("info", "     - [Job %s] Model already exists. Skipping download.", job.ID)
		return []byte(fmt.Sprintf("Model '%s' already exists at %s", fileName, destPath)), nil
	}

	downloadModelBeforeOpen()

	// Create the file through an os.Root on the cache root, exclusively:
	// the open cannot follow a symlink out of the root, and O_EXCL refuses
	// anything (including a symlink) that appeared at the leaf after the
	// checks above. curl only ever writes to this descriptor.
	root, err := os.OpenRoot(filepath.Dir(destDir))
	if err != nil {
		return nil, errModelFileCreate
	}
	defer root.Close()
	rel := filepath.Join(modelType, fileName)
	f, err := root.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, ErrUnsafeModelPath
		}
		return nil, errModelFileCreate
	}

	ctx.Log("info", "     - [Job %s] Starting download...", job.ID)
	out, runErr := runModelDownload(ctx.Context(), modelDownloadCurlArgs(fullURL), f)
	closeErr := f.Close()
	if runErr != nil || closeErr != nil {
		_ = root.Remove(rel)
		if runErr == nil {
			runErr = closeErr
		}
		return out, fmt.Errorf("model download failed: %w", runErr)
	}
	return []byte(fmt.Sprintf("Model '%s' downloaded to %s", fileName, destPath)), nil
}
