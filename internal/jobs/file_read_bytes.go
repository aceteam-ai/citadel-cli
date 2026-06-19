// internal/jobs/file_read_bytes.go
package jobs

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// defaultMaxReadBytes caps a single FILE_READ_BYTES read at 50 MB, matching the
// server-side cap. Used when the job payload omits or empties 'max_bytes'.
const defaultMaxReadBytes int64 = 50 * 1024 * 1024

// FileReadBytesHandler handles FILE_READ_BYTES jobs.
// Unlike FILE_READ, it reads a file as raw bytes (binary-safe: no line numbers,
// no binary rejection) and returns the content base64-encoded. This powers
// file-by-reference email attachments and upload_file ingestion, where the
// faithful bytes of PDFs/xlsx/CSV-with-NULs must cross the VPN mesh intact.
type FileReadBytesHandler struct {
	WorkspaceDir string
}

// NewFileReadBytesHandler creates a new FileReadBytesHandler rooted at workspace.
func NewFileReadBytesHandler(workspace string) *FileReadBytesHandler {
	return &FileReadBytesHandler{WorkspaceDir: workspace}
}

// Execute reads the requested file as raw bytes and returns it base64-encoded.
//
// Payload fields (all strings via nexus.Job):
//   - path: absolute or workspace-relative path to read
//   - max_bytes: server's size cap as a decimal string (default 50 MB)
//
// Response JSON:
//   - encoding: always "base64" (the marker the coordinator checks)
//   - content: standard base64 of the raw file bytes
//   - size: decoded byte length (raw file size)
func (h *FileReadBytesHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	path, ok := job.Payload["path"]
	if !ok || path == "" {
		return nil, fmt.Errorf("job payload missing 'path' field")
	}

	validated, err := ValidatePath(h.WorkspaceDir, path)
	if err != nil {
		return nil, fmt.Errorf("path validation failed: %w", err)
	}

	maxBytes := defaultMaxReadBytes
	if v, ok := job.Payload["max_bytes"]; ok && v != "" {
		maxBytes, err = strconv.ParseInt(v, 10, 64)
		if err != nil || maxBytes <= 0 {
			return nil, fmt.Errorf("invalid max_bytes: %q", v)
		}
	}

	info, err := os.Stat(validated)
	if err != nil {
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("path is a directory, not a file")
	}

	// Enforce the size cap before reading so we never load a huge file just to
	// reject it. The coordinator independently re-checks the decoded length.
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("file size %d exceeds max_bytes %d", info.Size(), maxBytes)
	}

	ctx.Log("info", "     - [Job %s] FILE_READ_BYTES %s (size=%d, max_bytes=%d)", job.ID, validated, info.Size(), maxBytes)

	data, err := os.ReadFile(validated)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	// Guard against a race where the file grew between Stat and ReadFile.
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file size %d exceeds max_bytes %d", len(data), maxBytes)
	}

	result := map[string]any{
		"encoding": "base64",
		"content":  base64.StdEncoding.EncodeToString(data),
		"size":     len(data),
	}
	return json.Marshal(result)
}

// Ensure FileReadBytesHandler implements JobHandler.
var _ JobHandler = (*FileReadBytesHandler)(nil)
