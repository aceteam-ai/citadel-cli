// internal/jobs/file_edit.go
package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// FileEditHandler handles FILE_EDIT jobs.
// It performs exact string replacement within a file, similar to Claude Code's
// Edit tool. By default the old_string must appear exactly once; set
// replace_all to replace every occurrence.
type FileEditHandler struct {
	WorkspaceDir string
}

// NewFileEditHandler creates a new FileEditHandler rooted at workspace.
func NewFileEditHandler(workspace string) *FileEditHandler {
	return &FileEditHandler{WorkspaceDir: workspace}
}

// Execute performs string replacement in the requested file.
//
// Payload fields (all strings via nexus.Job):
//   - path: absolute or workspace-relative path to edit
//   - old_string: the exact text to find
//   - new_string: the replacement text
//   - replace_all: "true" to replace all occurrences (default "false")
func (h *FileEditHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	path, ok := job.Payload["path"]
	if !ok || path == "" {
		return nil, fmt.Errorf("job payload missing 'path' field")
	}

	oldStr, ok := job.Payload["old_string"]
	if !ok {
		return nil, fmt.Errorf("job payload missing 'old_string' field")
	}
	if oldStr == "" {
		return nil, fmt.Errorf("old_string must not be empty")
	}

	newStr := job.Payload["new_string"] // May be empty (deletion).

	replaceAll := false
	if v, ok := job.Payload["replace_all"]; ok && v != "" {
		replaceAll, _ = strconv.ParseBool(v)
	}

	validated, err := ValidatePath(h.WorkspaceDir, path)
	if err != nil {
		return nil, fmt.Errorf("path validation failed: %w", err)
	}

	ctx.Log("info", "     - [Job %s] FILE_EDIT %s (replace_all=%v)", job.ID, validated, replaceAll)

	data, err := os.ReadFile(validated)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	content := string(data)
	count := strings.Count(content, oldStr)

	if count == 0 {
		return nil, fmt.Errorf("old_string not found in file")
	}
	if count > 1 && !replaceAll {
		return nil, fmt.Errorf("old_string found %d times; use replace_all=true or provide more context to make it unique", count)
	}

	var newContent string
	if replaceAll {
		newContent = strings.ReplaceAll(content, oldStr, newStr)
	} else {
		newContent = strings.Replace(content, oldStr, newStr, 1)
	}

	if err := os.WriteFile(validated, []byte(newContent), 0644); err != nil {
		return nil, fmt.Errorf("failed to write file: %w", err)
	}

	// Provide context around the edit: find the line range of the replacement.
	lines := strings.Split(newContent, "\n")
	editStart, editEnd := findEditContext(lines, newStr, 3)

	var sb strings.Builder
	for i := editStart; i <= editEnd && i < len(lines); i++ {
		fmt.Fprintf(&sb, "%6d\t%s\n", i+1, lines[i])
	}

	result := map[string]any{
		"path":         validated,
		"replacements": count,
		"context":      sb.String(),
	}
	return json.Marshal(result)
}

// findEditContext returns line indices (0-based) that provide context lines
// around the first occurrence of needle in the given lines.
func findEditContext(lines []string, needle string, contextLines int) (int, int) {
	joined := ""
	lineStarts := make([]int, len(lines))
	for i, line := range lines {
		lineStarts[i] = len(joined)
		joined += line
		if i < len(lines)-1 {
			joined += "\n"
		}
	}

	idx := strings.Index(joined, needle)
	if idx < 0 {
		// Fallback: show first few lines.
		end := contextLines*2 + 1
		if end >= len(lines) {
			end = len(lines) - 1
		}
		return 0, end
	}

	// Find which line the match starts on.
	matchLine := 0
	for i, start := range lineStarts {
		if start > idx {
			break
		}
		matchLine = i
	}

	start := matchLine - contextLines
	if start < 0 {
		start = 0
	}

	// Estimate the end line from needle length.
	needleLines := strings.Count(needle, "\n")
	end := matchLine + needleLines + contextLines
	if end >= len(lines) {
		end = len(lines) - 1
	}

	return start, end
}

// Ensure FileEditHandler implements JobHandler.
var _ JobHandler = (*FileEditHandler)(nil)
