// internal/jobs/file_read_bytes.go
package jobs

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// defaultMaxReadBytes caps a single FILE_READ_BYTES read at 50 MiB, even when
// the job payload requests a larger max_bytes.
const defaultMaxReadBytes int64 = 50 * 1024 * 1024

// maxReadChunkBytes keeps one encoded job result comfortably below the mesh
// response envelope. Whole-file callers retain the existing 50 MB contract;
// callers that opt into byte ranges must use bounded chunks.
const maxReadChunkBytes int64 = 8 * 1024 * 1024

// FileReadBytesHandler handles FILE_READ_BYTES jobs.
// Unlike FILE_READ, it reads a file as raw bytes (binary-safe: no line numbers,
// no binary rejection) and returns the content base64-encoded. This powers
// file-by-reference email attachments and upload_file ingestion, where the
// faithful bytes of PDFs/xlsx/CSV-with-NULs must cross the VPN mesh intact.
type FileReadBytesHandler struct {
	WorkspaceDir string
	// AllowOutsideWorkspace, when true, permits reading files outside the
	// workspace sandbox. Bounded by OS file permissions and size caps.
	AllowOutsideWorkspace bool
}

// NewFileReadBytesHandler creates a new FileReadBytesHandler rooted at workspace.
func NewFileReadBytesHandler(workspace string) *FileReadBytesHandler {
	return &FileReadBytesHandler{WorkspaceDir: workspace}
}

// Execute reads the requested file as raw bytes and returns it base64-encoded.
//
// Payload fields (all strings via nexus.Job):
//   - path: absolute or workspace-relative path to read
//   - max_bytes: requested size cap as a decimal string (hard limit 50 MiB)
//   - offset: optional zero-based byte offset; requires length
//   - length: optional requested byte count, capped at 8 MB; requires offset
//
// Response JSON:
//   - encoding: always "base64" (the marker the coordinator checks)
//   - content: standard base64 of the raw file bytes
//   - size: decoded byte length returned in this response
//   - offset: returned byte offset (range reads only)
//   - total_size: full file size (range reads only)
//   - eof: whether the returned chunk reaches the file end (range reads only)
func (h *FileReadBytesHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	path, ok := job.Payload["path"]
	if !ok || path == "" {
		return nil, fmt.Errorf("job payload missing 'path' field")
	}

	validated, err := ValidateReadPath(h.WorkspaceDir, path, h.AllowOutsideWorkspace)
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
	if maxBytes > defaultMaxReadBytes {
		maxBytes = defaultMaxReadBytes
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

	offsetRaw, hasOffset := job.Payload["offset"]
	lengthRaw, hasLength := job.Payload["length"]
	if hasOffset != hasLength {
		return nil, fmt.Errorf("offset and length must be provided together")
	}
	if hasOffset {
		offset, parseErr := strconv.ParseInt(offsetRaw, 10, 64)
		if parseErr != nil || offset < 0 {
			return nil, fmt.Errorf("invalid offset: %q", offsetRaw)
		}
		length, parseErr := strconv.ParseInt(lengthRaw, 10, 64)
		if parseErr != nil || length <= 0 || length > maxReadChunkBytes {
			return nil, fmt.Errorf("invalid length: %q (must be between 1 and %d)", lengthRaw, maxReadChunkBytes)
		}
		if offset > info.Size() {
			return nil, fmt.Errorf("offset %d exceeds file size %d", offset, info.Size())
		}

		remaining := info.Size() - offset
		readLength := length
		if remaining < readLength {
			readLength = remaining
		}
		data := make([]byte, readLength)
		if readLength > 0 {
			file, openErr := os.Open(validated)
			if openErr != nil {
				return nil, fmt.Errorf("failed to open file: %w", openErr)
			}
			defer file.Close()

			bytesRead, readErr := file.ReadAt(data, offset)
			if readErr != nil && readErr != io.EOF {
				return nil, fmt.Errorf("failed to read file range: %w", readErr)
			}
			data = data[:bytesRead]
		}

		ctx.Log("info", "     - [Job %s] FILE_READ_BYTES %s (offset=%d, size=%d, total_size=%d, max_bytes=%d)", job.ID, validated, offset, len(data), info.Size(), maxBytes)
		result := map[string]any{
			"encoding":   "base64",
			"content":    base64.StdEncoding.EncodeToString(data),
			"size":       len(data),
			"offset":     offset,
			"total_size": info.Size(),
			"eof":        offset+int64(len(data)) >= info.Size(),
		}
		return json.Marshal(result)
	}

	ctx.Log("info", "     - [Job %s] FILE_READ_BYTES %s (size=%d, max_bytes=%d)", job.ID, validated, info.Size(), maxBytes)

	file, err := os.Open(validated)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
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
