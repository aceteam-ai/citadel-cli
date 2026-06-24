// internal/jobs/file_write_bytes.go
package jobs

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// defaultMaxWriteBytes caps a single FILE_WRITE_BYTES write at 50 MB, matching
// the FILE_READ_BYTES read cap. Used when the payload omits 'max_bytes'.
const defaultMaxWriteBytes int64 = 50 * 1024 * 1024

// FileWriteBytesHandler handles FILE_WRITE_BYTES jobs.
//
// Unlike FILE_WRITE (which writes the payload string verbatim and is therefore
// text-only), this handler base64-decodes the payload content into raw bytes
// before writing. It is the binary-safe counterpart to FileReadBytesHandler and
// powers uploads of recorded meeting audio (webm/wav) into the node workspace,
// where the TRANSCRIBE_AUDIO handler then picks them up. Audio bytes must cross
// the Redis wire intact, which the text FILE_WRITE path cannot guarantee.
type FileWriteBytesHandler struct {
	WorkspaceDir string
}

// NewFileWriteBytesHandler creates a handler rooted at workspace.
func NewFileWriteBytesHandler(workspace string) *FileWriteBytesHandler {
	return &FileWriteBytesHandler{WorkspaceDir: workspace}
}

// Execute base64-decodes the payload content and writes the raw bytes to disk.
//
// Payload fields (all strings via nexus.Job):
//   - path:      absolute or workspace-relative path to write.
//   - content:   standard base64 of the raw bytes to write.
//   - max_bytes: decoded-size cap as a decimal string (default 50 MB).
//
// Response JSON:
//   - path:          validated absolute path written.
//   - bytes_written: decoded byte length actually written.
func (h *FileWriteBytesHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	path, ok := job.Payload["path"]
	if !ok || path == "" {
		return nil, fmt.Errorf("job payload missing 'path' field")
	}

	encoded, ok := job.Payload["content"]
	if !ok {
		return nil, fmt.Errorf("job payload missing 'content' field")
	}

	validated, err := ValidatePath(h.WorkspaceDir, path)
	if err != nil {
		return nil, fmt.Errorf("path validation failed: %w", err)
	}

	maxBytes := defaultMaxWriteBytes
	if v, ok := job.Payload["max_bytes"]; ok && v != "" {
		// Mirror FILE_READ_BYTES: strconv.ParseInt with a positive-value check.
		parsed, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil || parsed <= 0 {
			return nil, fmt.Errorf("invalid max_bytes: %q", v)
		}
		maxBytes = parsed
	}

	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("content is not valid base64: %w", err)
	}

	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("decoded size %d exceeds max_bytes %d", len(data), maxBytes)
	}

	ctx.Log("info", "     - [Job %s] FILE_WRITE_BYTES %s (%d bytes)", job.ID, validated, len(data))

	dir := filepath.Dir(validated)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create parent directories: %w", err)
	}

	if err := os.WriteFile(validated, data, 0644); err != nil {
		return nil, fmt.Errorf("failed to write file: %w", err)
	}

	result := map[string]any{
		"path":          validated,
		"bytes_written": len(data),
	}
	return json.Marshal(result)
}

// Ensure FileWriteBytesHandler implements JobHandler.
var _ JobHandler = (*FileWriteBytesHandler)(nil)
