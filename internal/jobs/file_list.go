// internal/jobs/file_list.go
package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// FileListHandler handles FILE_LIST jobs.
// It lists directory contents within the configured workspace, optionally
// filtering by a glob pattern.
type FileListHandler struct {
	WorkspaceDir string
	// AllowOutsideWorkspace, when true, permits listing directories outside
	// the workspace sandbox. Bounded by OS file permissions.
	AllowOutsideWorkspace bool
}

// NewFileListHandler creates a new FileListHandler rooted at workspace.
func NewFileListHandler(workspace string) *FileListHandler {
	return &FileListHandler{WorkspaceDir: workspace}
}

// fileEntry represents one item in a directory listing.
type fileEntry struct {
	Name  string `json:"name"`
	Type  string `json:"type"` // "file", "dir", "symlink"
	Size  int64  `json:"size"`
	Mode  string `json:"mode"`
}

// maxListResults caps directory listings to prevent huge responses.
const maxListResults = 5000

// Execute lists the contents of the requested directory.
//
// Payload fields (all strings via nexus.Job):
//   - path: absolute or workspace-relative directory path
//   - pattern: optional glob pattern to filter entries (e.g. "*.go")
func (h *FileListHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	path, ok := job.Payload["path"]
	if !ok || path == "" {
		return nil, fmt.Errorf("job payload missing 'path' field")
	}

	pattern := job.Payload["pattern"] // May be empty.

	validated, err := ValidateReadPath(h.WorkspaceDir, path, h.AllowOutsideWorkspace)
	if err != nil {
		return nil, fmt.Errorf("path validation failed: %w", err)
	}

	info, err := os.Stat(validated)
	if err != nil {
		return nil, fmt.Errorf("cannot stat path: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("path is not a directory: %s", validated)
	}

	ctx.Log("info", "     - [Job %s] FILE_LIST %s (pattern=%q)", job.ID, validated, pattern)

	entries, err := os.ReadDir(validated)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory: %w", err)
	}

	var results []fileEntry
	for _, entry := range entries {
		name := entry.Name()

		// Apply glob filter if provided.
		if pattern != "" {
			matched, err := filepath.Match(pattern, name)
			if err != nil {
				return nil, fmt.Errorf("invalid glob pattern %q: %w", pattern, err)
			}
			if !matched {
				continue
			}
		}

		info, err := entry.Info()
		if err != nil {
			continue // Skip entries we can't stat.
		}

		entryType := "file"
		if entry.IsDir() {
			entryType = "dir"
		} else if entry.Type()&os.ModeSymlink != 0 {
			entryType = "symlink"
		}

		results = append(results, fileEntry{
			Name: name,
			Type: entryType,
			Size: info.Size(),
			Mode: info.Mode().String(),
		})

		if len(results) >= maxListResults {
			break
		}
	}

	out := map[string]any{
		"path":    validated,
		"entries": results,
		"count":   len(results),
	}
	if len(results) >= maxListResults {
		out["truncated"] = true
	}
	return json.Marshal(out)
}

// Ensure FileListHandler implements JobHandler.
var _ JobHandler = (*FileListHandler)(nil)
