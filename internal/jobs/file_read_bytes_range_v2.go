package jobs

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/aceteam-ai/citadel-cli/internal/artifact"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

type rangeDescriptor interface {
	artifact.Statter
	ReadAt([]byte, int64) (int, error)
	Close() error
}

// FileReadBytesRangeV2Handler serves conditional reads from one opened file.
// Refusal outcomes are JSON results so the legacy adapter preserves their type.
type FileReadBytesRangeV2Handler struct {
	WorkspaceDir          string
	AllowOutsideWorkspace bool
	AuthorizedRoots       func() []string
	openFile              func(string) (rangeDescriptor, error)
}

func NewFileReadBytesRangeV2Handler(workspace string) *FileReadBytesRangeV2Handler {
	return &FileReadBytesRangeV2Handler{WorkspaceDir: workspace}
}

// Open through an authorized root as well as validating the path. A symlink
// replacement between validation and open must not escape that authorization.
func (h *FileReadBytesRangeV2Handler) openAuthorizedFile(validated string, roots []string) (rangeDescriptor, error) {
	if h.AllowOutsideWorkspace {
		return os.Open(validated)
	}
	// Compare target and root in resolved coordinates. The actual file open
	// still goes through os.Root so a raced symlink cannot escape the root.
	resolvedTarget, err := filepath.EvalSymlinks(validated)
	if err != nil {
		return nil, err
	}
	for _, rootPath := range append([]string{h.WorkspaceDir}, roots...) {
		absolute, err := filepath.Abs(rootPath)
		if err != nil {
			continue
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(resolved, resolvedTarget)
		if err != nil || !filepath.IsLocal(relative) {
			continue
		}
		root, err := os.OpenRoot(resolved)
		if err != nil {
			return nil, err
		}
		file, openErr := root.Open(relative)
		closeErr := root.Close()
		if openErr != nil {
			return nil, openErr
		}
		if closeErr != nil {
			file.Close()
			return nil, closeErr
		}
		return file, nil
	}
	return nil, fmt.Errorf("path has no authorized root")
}

func safeRangeInteger(payload map[string]string, field string) (int64, error) {
	raw, ok := payload[field]
	if !ok || raw == "" {
		return 0, fmt.Errorf("%s is required", field)
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid %s", field)
		}
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n > artifact.MaxSafeInteger {
		return 0, fmt.Errorf("invalid %s", field)
	}
	return n, nil
}

func (h *FileReadBytesRangeV2Handler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	if err := ctx.Context().Err(); err != nil {
		return nil, err
	}
	p := job.Payload
	if p["path"] == "" {
		return nil, fmt.Errorf("job payload missing 'path' field")
	}
	if p["expected_artifact_version"] != "1" || !artifact.ValidSHA256(p["expected_sha256"]) || !artifact.ValidGeneration(p["expected_generation"]) {
		return nil, fmt.Errorf("missing or unsupported expected artifact identity")
	}
	expectedBytes, err := safeRangeInteger(p, "expected_bytes")
	if err != nil {
		return nil, err
	}
	offset, err := safeRangeInteger(p, "offset")
	if err != nil {
		return nil, err
	}
	length, err := safeRangeInteger(p, "length")
	if err != nil || length <= 0 || length > maxReadChunkBytes {
		return nil, fmt.Errorf("invalid length")
	}
	if _, ok := p["max_bytes"]; ok {
		cap, capErr := safeRangeInteger(p, "max_bytes")
		if capErr != nil || cap <= 0 || cap > maxReadChunkBytes || length > cap {
			return nil, fmt.Errorf("invalid max_bytes")
		}
	}
	roots := resolveAuthorizedRoots(h.AuthorizedRoots)
	validated, err := ValidateReadPathWithRoots(h.WorkspaceDir, roots, p["path"], h.AllowOutsideWorkspace)
	if err != nil {
		return nil, fmt.Errorf("path validation failed: %w", err)
	}
	open := h.openFile
	if open == nil {
		open = func(path string) (rangeDescriptor, error) { return h.openAuthorizedFile(path, roots) }
	}
	file, err := open(validated)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()
	before, err := artifact.Capture(file)
	if err != nil {
		return nil, fmt.Errorf("failed to capture artifact identity: %w", err)
	}
	refusal := func() ([]byte, error) {
		return json.Marshal(map[string]any{"contract_version": 2, "outcome": "precondition_failed"})
	}
	generation, err := before.Generation(p["expected_sha256"])
	if err != nil {
		return nil, err
	}
	if before.Bytes != expectedBytes || generation != p["expected_generation"] {
		return refusal()
	}
	identity := map[string]any{"version": 1, "sha256": p["expected_sha256"], "bytes": expectedBytes, "generation": generation}
	verify := func() (bool, error) {
		after, statErr := artifact.Capture(file)
		if statErr != nil {
			return false, statErr
		}
		return before == after, nil
	}
	if offset >= before.Bytes {
		stable, err := verify()
		if err != nil {
			return nil, fmt.Errorf("failed to verify artifact identity: %w", err)
		}
		if !stable {
			return refusal()
		}
		return json.Marshal(map[string]any{"contract_version": 2, "outcome": "range_not_satisfiable", "artifact": identity, "offset": offset, "total_size": before.Bytes})
	}
	readLength := length
	if remaining := before.Bytes - offset; readLength > remaining {
		readLength = remaining
	}
	data := make([]byte, int(readLength))
	n, readErr := file.ReadAt(data, offset)
	if err := ctx.Context().Err(); err != nil {
		return nil, err
	}
	stable, err := verify()
	if err != nil {
		return nil, fmt.Errorf("failed to verify artifact identity: %w", err)
	}
	if !stable {
		return refusal()
	}
	if readErr != nil && readErr != io.EOF {
		return nil, fmt.Errorf("failed to read file range: %w", readErr)
	}
	if int64(n) != readLength || readErr == io.EOF {
		return refusal()
	}
	return json.Marshal(map[string]any{
		"contract_version": 2, "outcome": "ok", "artifact": identity,
		"encoding": "base64", "content": base64.StdEncoding.EncodeToString(data),
		"offset": offset, "size": n, "total_size": before.Bytes, "eof": offset+int64(n) == before.Bytes,
	})
}

var _ JobHandler = (*FileReadBytesRangeV2Handler)(nil)
