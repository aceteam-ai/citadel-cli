// internal/jobs/file_search.go
package jobs

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// FileSearchHandler handles FILE_SEARCH jobs.
// It performs grep-like text search across files in the workspace, returning
// matches with line numbers and context.
type FileSearchHandler struct {
	WorkspaceDir string
	// AllowOutsideWorkspace, when true, permits searching files outside the
	// workspace sandbox. Bounded by OS file permissions and size caps.
	AllowOutsideWorkspace bool
}

// NewFileSearchHandler creates a new FileSearchHandler rooted at workspace.
func NewFileSearchHandler(workspace string) *FileSearchHandler {
	return &FileSearchHandler{WorkspaceDir: workspace}
}

// searchMatch represents one grep-like match.
type searchMatch struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Content string `json:"content"`
}

// maxSearchResults caps search output to prevent huge responses.
const maxSearchResults = 500

// maxSearchFiles caps the number of files scanned.
const maxSearchFiles = 10000

// Execute searches for a text query across files in the given directory.
//
// Payload fields (all strings via nexus.Job):
//   - path: root directory to search (absolute or workspace-relative)
//   - query: the text to search for (case-sensitive substring match)
//   - file_pattern: optional glob pattern to filter filenames (e.g. "*.go")
func (h *FileSearchHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	path, ok := job.Payload["path"]
	if !ok || path == "" {
		return nil, fmt.Errorf("job payload missing 'path' field")
	}

	query, ok := job.Payload["query"]
	if !ok || query == "" {
		return nil, fmt.Errorf("job payload missing 'query' field")
	}

	filePattern := job.Payload["file_pattern"] // May be empty.

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

	ctx.Log("info", "     - [Job %s] FILE_SEARCH %s query=%q pattern=%q", job.ID, validated, query, filePattern)

	var matches []searchMatch
	filesScanned := 0
	truncated := false

	err = filepath.WalkDir(validated, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // Skip unreadable entries.
		}
		if d.IsDir() {
			// Skip common noise directories.
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "__pycache__" || name == ".venv" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}

		if filesScanned >= maxSearchFiles {
			truncated = true
			return filepath.SkipAll
		}

		// Apply file pattern filter.
		if filePattern != "" {
			matched, err := filepath.Match(filePattern, d.Name())
			if err != nil {
				return nil // Skip bad pattern silently.
			}
			if !matched {
				return nil
			}
		}

		// Skip binary files by checking the first chunk.
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()

		filesScanned++

		// Read first 512 bytes to detect binary.
		header := make([]byte, 512)
		n, _ := f.Read(header)
		if n > 0 && isBinaryContent(header[:n]) {
			return nil
		}

		// Seek back to start for full scan.
		if _, err := f.Seek(0, 0); err != nil {
			return nil
		}

		scanner := bufio.NewScanner(f)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			line := scanner.Text()
			if strings.Contains(line, query) {
				// Use workspace-relative path for cleaner output.
				// When searching outside the workspace, fall back to the
				// absolute path instead of emitting "../../../..." strings.
				relPath, err := filepath.Rel(h.WorkspaceDir, p)
				if err != nil || strings.HasPrefix(relPath, "..") {
					relPath = p
				}
				matches = append(matches, searchMatch{
					File:    relPath,
					Line:    lineNum,
					Content: truncateLine(line, 200),
				})
				if len(matches) >= maxSearchResults {
					truncated = true
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}

	out := map[string]any{
		"matches":       matches,
		"match_count":   len(matches),
		"files_scanned": filesScanned,
	}
	if truncated {
		out["truncated"] = true
	}
	return json.Marshal(out)
}

// truncateLine shortens a line to maxLen characters, appending "..." if
// truncated.
func truncateLine(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// Ensure FileSearchHandler implements JobHandler.
var _ JobHandler = (*FileSearchHandler)(nil)
