package jobs

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fileReadBytesResult mirrors the JSON contract the coordinator decodes.
type fileReadBytesResult struct {
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
	Size     int    `json:"size"`
}

func runFileReadBytes(t *testing.T, ws string, payload map[string]string) fileReadBytesResult {
	t.Helper()
	h := NewFileReadBytesHandler(ws)
	out, err := h.Execute(JobContext{}, makeJob(payload))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var result fileReadBytesResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	return result
}

// TestFileReadBytes_BinaryRoundTrip is the core proof that binary is no longer
// rejected: a file containing NUL bytes round-trips faithfully through base64.
func TestFileReadBytes_BinaryRoundTrip(t *testing.T) {
	ws := setupWorkspace(t)
	raw := []byte{0x89, 0x50, 0x4E, 0x47, 0x00, 0x0D, 0x0A, 0x00, 0xFF, 0x00}
	path := filepath.Join(ws, "binary.dat")
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result := runFileReadBytes(t, ws, map[string]string{"path": path})

	if result.Encoding != "base64" {
		t.Errorf("encoding = %q, want \"base64\"", result.Encoding)
	}
	if result.Size != len(raw) {
		t.Errorf("size = %d, want %d", result.Size, len(raw))
	}
	decoded, err := base64.StdEncoding.DecodeString(result.Content)
	if err != nil {
		t.Fatalf("content is not valid standard base64: %v", err)
	}
	if !bytes.Equal(decoded, raw) {
		t.Errorf("decoded bytes %v != original %v", decoded, raw)
	}
}

func TestFileReadBytes_TextRoundTrip(t *testing.T) {
	ws := setupWorkspace(t)
	content := "line one\nline two\nline three\n"
	path := writeTestFile(t, ws, "test.txt", content)

	result := runFileReadBytes(t, ws, map[string]string{"path": path})

	decoded, err := base64.StdEncoding.DecodeString(result.Content)
	if err != nil {
		t.Fatalf("content is not valid standard base64: %v", err)
	}
	if string(decoded) != content {
		t.Errorf("decoded = %q, want %q", string(decoded), content)
	}
	// Crucially, no cat -n line numbering is applied.
	if strings.Contains(string(decoded), "\t") {
		t.Errorf("decoded content unexpectedly contains a tab (line numbering?): %q", string(decoded))
	}
}

func TestFileReadBytes_MaxBytesCap(t *testing.T) {
	ws := setupWorkspace(t)
	path := writeTestFile(t, ws, "big.bin", strings.Repeat("A", 100))

	h := NewFileReadBytesHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":      path,
		"max_bytes": "10",
	}))
	if err == nil {
		t.Fatal("expected error when file exceeds max_bytes, got nil")
	}
	if !strings.Contains(err.Error(), "max_bytes") {
		t.Errorf("error should mention max_bytes, got: %v", err)
	}
}

func TestFileReadBytes_MaxBytesAllowsWithinCap(t *testing.T) {
	ws := setupWorkspace(t)
	raw := strings.Repeat("A", 100)
	path := writeTestFile(t, ws, "ok.bin", raw)

	result := runFileReadBytes(t, ws, map[string]string{
		"path":      path,
		"max_bytes": fmt.Sprintf("%d", len(raw)),
	})
	if result.Size != len(raw) {
		t.Errorf("size = %d, want %d", result.Size, len(raw))
	}
}

func TestFileReadBytes_InvalidMaxBytes(t *testing.T) {
	ws := setupWorkspace(t)
	path := writeTestFile(t, ws, "f.txt", "hi")

	h := NewFileReadBytesHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":      path,
		"max_bytes": "not-a-number",
	}))
	if err == nil {
		t.Fatal("expected error for invalid max_bytes, got nil")
	}
}

func TestFileReadBytes_PathTraversal(t *testing.T) {
	ws := setupWorkspace(t)
	h := NewFileReadBytesHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path": "/etc/passwd",
	}))
	if err == nil {
		t.Fatal("expected error for path outside workspace, got nil")
	}
	if !strings.Contains(err.Error(), "path validation") {
		t.Errorf("error should mention path validation, got: %v", err)
	}
}

func TestFileReadBytes_RejectsDirectory(t *testing.T) {
	ws := setupWorkspace(t)
	sub := filepath.Join(ws, "subdir")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	h := NewFileReadBytesHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{"path": sub}))
	if err == nil {
		t.Fatal("expected error when reading a directory, got nil")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error should mention directory, got: %v", err)
	}
}

func TestFileReadBytes_MissingPath(t *testing.T) {
	ws := setupWorkspace(t)
	h := NewFileReadBytesHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{}))
	if err == nil {
		t.Fatal("expected error for missing path, got nil")
	}
}

func TestFileReadBytes_NonExistent(t *testing.T) {
	ws := setupWorkspace(t)
	h := NewFileReadBytesHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path": filepath.Join(ws, "nope.dat"),
	}))
	if err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
}
