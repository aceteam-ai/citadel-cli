package jobs

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

const fileReadRangeVersion = 1

// FileReadBytesRangeHandler serves only the versioned, bounded range job type.
// An older node does not register this type and cannot interpret it as a
// whole-file FILE_READ_BYTES request.
type FileReadBytesRangeHandler struct {
	WorkspaceDir          string
	AllowOutsideWorkspace bool
}

func NewFileReadBytesRangeHandler(workspace string) *FileReadBytesRangeHandler {
	return &FileReadBytesRangeHandler{WorkspaceDir: workspace}
}

func parseFileReadRange(payload map[string]string, totalSize int64) (int64, int64, error) {
	offsetRaw, hasOffset := payload["offset"]
	lengthRaw, hasLength := payload["length"]
	if !hasOffset || !hasLength {
		return 0, 0, fmt.Errorf("offset and length are required")
	}
	offset, err := strconv.ParseInt(offsetRaw, 10, 64)
	if err != nil || offset < 0 || offset > totalSize {
		return 0, 0, fmt.Errorf("invalid offset: %q", offsetRaw)
	}
	length, err := strconv.ParseInt(lengthRaw, 10, 64)
	if err != nil || length <= 0 || length > maxReadChunkBytes || offset > math.MaxInt64-length {
		return 0, 0, fmt.Errorf("invalid length: %q", lengthRaw)
	}
	if maxRaw, ok := payload["max_bytes"]; ok {
		maxBytes, parseErr := strconv.ParseInt(maxRaw, 10, 64)
		if parseErr != nil || maxBytes <= 0 || maxBytes > maxReadChunkBytes || length > maxBytes {
			return 0, 0, fmt.Errorf("invalid max_bytes: %q", maxRaw)
		}
	}
	remaining := totalSize - offset
	if length > remaining {
		length = remaining
	}
	return offset, length, nil
}

// Execute returns a base64 range with contract_version, offset, size,
// total_size, and eof. Decoded bytes never exceed maxReadChunkBytes, regardless
// of the file's total size or any caller-provided max_bytes value.
func (h *FileReadBytesRangeHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	path := job.Payload["path"]
	if path == "" {
		return nil, fmt.Errorf("job payload missing 'path' field")
	}
	validated, err := ValidateReadPath(h.WorkspaceDir, path, h.AllowOutsideWorkspace)
	if err != nil {
		return nil, fmt.Errorf("path validation failed: %w", err)
	}
	file, err := os.Open(validated)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() < 0 {
		return nil, fmt.Errorf("path is not a regular file")
	}
	offset, readLength, err := parseFileReadRange(job.Payload, info.Size())
	if err != nil {
		return nil, err
	}
	data := make([]byte, int(readLength))
	if readLength > 0 {
		bytesRead, readErr := file.ReadAt(data, offset)
		if readErr != nil && readErr != io.EOF {
			return nil, fmt.Errorf("failed to read file range: %w", readErr)
		}
		if int64(bytesRead) != readLength {
			return nil, fmt.Errorf("file changed while reading range")
		}
	}
	ctx.Log("info", "     - [Job %s] FILE_READ_BYTES_RANGE_V1 %s (offset=%d, size=%d, total_size=%d)", job.ID, validated, offset, len(data), info.Size())
	return json.Marshal(map[string]any{
		"contract_version": fileReadRangeVersion,
		"encoding":         "base64",
		"content":          base64.StdEncoding.EncodeToString(data),
		"offset":           offset,
		"size":             len(data),
		"total_size":       info.Size(),
		"eof":              offset+int64(len(data)) == info.Size(),
	})
}

var _ JobHandler = (*FileReadBytesRangeHandler)(nil)
