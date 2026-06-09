// internal/jobs/file_write.go
package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// FileWriteHandler handles FILE_WRITE jobs.
// It writes content to a file within the configured workspace, creating
// parent directories as needed.
type FileWriteHandler struct {
	WorkspaceDir string
}

// NewFileWriteHandler creates a new FileWriteHandler rooted at workspace.
func NewFileWriteHandler(workspace string) *FileWriteHandler {
	return &FileWriteHandler{WorkspaceDir: workspace}
}

// Execute writes the provided content to the requested file.
//
// Payload fields (all strings via nexus.Job):
//   - path: absolute or workspace-relative path to write
//   - content: the text content to write
func (h *FileWriteHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	path, ok := job.Payload["path"]
	if !ok || path == "" {
		return nil, fmt.Errorf("job payload missing 'path' field")
	}

	content, ok := job.Payload["content"]
	if !ok {
		return nil, fmt.Errorf("job payload missing 'content' field")
	}

	validated, err := ValidatePath(h.WorkspaceDir, path)
	if err != nil {
		return nil, fmt.Errorf("path validation failed: %w", err)
	}

	ctx.Log("info", "     - [Job %s] FILE_WRITE %s (%d bytes)", job.ID, validated, len(content))

	// Create parent directories if they don't exist.
	dir := filepath.Dir(validated)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create parent directories: %w", err)
	}

	if err := os.WriteFile(validated, []byte(content), 0644); err != nil {
		return nil, fmt.Errorf("failed to write file: %w", err)
	}

	result := map[string]any{
		"path":          validated,
		"bytes_written": len(content),
	}
	return json.Marshal(result)
}

// Ensure FileWriteHandler implements JobHandler.
var _ JobHandler = (*FileWriteHandler)(nil)
