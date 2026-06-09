// internal/jobs/file_read.go
package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// FileReadHandler handles FILE_READ jobs.
// It reads a file within the configured workspace and returns its content
// with line numbers (similar to cat -n).
type FileReadHandler struct {
	WorkspaceDir string
}

// NewFileReadHandler creates a new FileReadHandler rooted at workspace.
func NewFileReadHandler(workspace string) *FileReadHandler {
	return &FileReadHandler{WorkspaceDir: workspace}
}

// Execute reads the requested file and returns content with line numbers.
//
// Payload fields (all strings via nexus.Job):
//   - path: absolute or workspace-relative path to read
//   - offset: starting line number (0-based, default "0")
//   - limit: max lines to return (default "2000")
func (h *FileReadHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	path, ok := job.Payload["path"]
	if !ok || path == "" {
		return nil, fmt.Errorf("job payload missing 'path' field")
	}

	validated, err := ValidatePath(h.WorkspaceDir, path)
	if err != nil {
		return nil, fmt.Errorf("path validation failed: %w", err)
	}

	offset := 0
	if v, ok := job.Payload["offset"]; ok && v != "" {
		offset, err = strconv.Atoi(v)
		if err != nil || offset < 0 {
			return nil, fmt.Errorf("invalid offset: %q", v)
		}
	}

	limit := 2000
	if v, ok := job.Payload["limit"]; ok && v != "" {
		limit, err = strconv.Atoi(v)
		if err != nil || limit <= 0 {
			return nil, fmt.Errorf("invalid limit: %q", v)
		}
	}

	ctx.Log("info", "     - [Job %s] FILE_READ %s (offset=%d, limit=%d)", job.ID, validated, offset, limit)

	data, err := os.ReadFile(validated)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	// Check for binary content (first 8KB).
	checkSize := 8192
	if len(data) < checkSize {
		checkSize = len(data)
	}
	if isBinaryContent(data[:checkSize]) {
		return nil, fmt.Errorf("file appears to be binary; use a different tool to inspect it")
	}

	lines := strings.Split(string(data), "\n")
	totalLines := len(lines)

	// Apply offset.
	if offset >= totalLines {
		result := map[string]any{
			"content":     "",
			"total_lines": totalLines,
			"offset":      offset,
			"limit":       limit,
		}
		return json.Marshal(result)
	}
	lines = lines[offset:]

	// Apply limit.
	if len(lines) > limit {
		lines = lines[:limit]
	}

	// Format with line numbers (1-based, matching cat -n).
	var sb strings.Builder
	for i, line := range lines {
		lineNum := offset + i + 1
		fmt.Fprintf(&sb, "%6d\t%s\n", lineNum, line)
	}

	result := map[string]any{
		"content":     sb.String(),
		"total_lines": totalLines,
		"offset":      offset,
		"limit":       limit,
	}
	return json.Marshal(result)
}

// Ensure FileReadHandler implements JobHandler.
var _ JobHandler = (*FileReadHandler)(nil)
