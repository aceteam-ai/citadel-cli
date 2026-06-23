package jobs

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- ValidateReadPath unit tests ---

func TestValidateReadPath_Sandboxed_DelegatesToValidatePath(t *testing.T) {
	ws := setupWorkspace(t)
	writeTestFile(t, ws, "hello.txt", "hello")

	got, err := ValidateReadPath(ws, filepath.Join(ws, "hello.txt"), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(got, "hello.txt") {
		t.Errorf("expected path ending in hello.txt, got %q", got)
	}
}

func TestValidateReadPath_Sandboxed_RejectsOutside(t *testing.T) {
	ws := setupWorkspace(t)

	_, err := ValidateReadPath(ws, "/etc/passwd", false)
	if err == nil {
		t.Fatal("expected error for path outside workspace with allowOutside=false")
	}
}

func TestValidateReadPath_Relaxed_AllowsOutside(t *testing.T) {
	ws := setupWorkspace(t)
	outside := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(outside)
	target := filepath.Join(resolved, "outside.txt")
	writeTestFile(t, resolved, "outside.txt", "data")

	got, err := ValidateReadPath(ws, target, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != target {
		t.Errorf("got %q, want %q", got, target)
	}
}

func TestValidateReadPath_Relaxed_RelativeAnchorsToWorkspace(t *testing.T) {
	ws := setupWorkspace(t)

	got, err := ValidateReadPath(ws, "sub/file.txt", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := filepath.Join(ws, "sub", "file.txt")
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestValidateReadPath_Relaxed_EmptyPath(t *testing.T) {
	ws := setupWorkspace(t)

	_, err := ValidateReadPath(ws, "", true)
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestValidateReadPath_Relaxed_CleansPath(t *testing.T) {
	ws := setupWorkspace(t)
	outside := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(outside)

	// Build a dirty path with /.. that should be cleaned to the resolved dir.
	dirty := filepath.Join(resolved, "sub", "..", "clean.txt")
	want := filepath.Join(resolved, "clean.txt")

	got, err := ValidateReadPath(ws, dirty, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q (cleaned)", got, want)
	}
}

func TestValidateReadPath_Relaxed_RelativeWithoutWorkspace(t *testing.T) {
	_, err := ValidateReadPath("", "relative/path.txt", true)
	if err == nil {
		t.Fatal("expected error for relative path without workspace")
	}
}

// --- FILE_READ with AllowOutsideWorkspace ---

func TestFileRead_OutsideWorkspace_Allowed(t *testing.T) {
	ws := setupWorkspace(t)
	outside := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(outside)
	path := writeTestFile(t, resolved, "external.txt", "external content\n")

	h := NewFileReadHandler(ws)
	h.AllowOutsideWorkspace = true
	out, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path": path,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	content := result["content"].(string)
	if !strings.Contains(content, "external content") {
		t.Errorf("content should contain 'external content', got: %s", content)
	}
}

func TestFileRead_OutsideWorkspace_Blocked_ByDefault(t *testing.T) {
	ws := setupWorkspace(t)
	outside := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(outside)
	path := writeTestFile(t, resolved, "external.txt", "external content\n")

	h := NewFileReadHandler(ws)
	// AllowOutsideWorkspace defaults to false
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path": path,
	}))
	if err == nil {
		t.Fatal("expected error for outside path with default config")
	}
	if !strings.Contains(err.Error(), "path validation") {
		t.Errorf("error should mention path validation, got: %v", err)
	}
}

// --- FILE_READ_BYTES with AllowOutsideWorkspace ---

func TestFileReadBytes_OutsideWorkspace_Allowed(t *testing.T) {
	ws := setupWorkspace(t)
	outside := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(outside)
	content := "binary-safe content"
	path := writeTestFile(t, resolved, "external.bin", content)

	h := NewFileReadBytesHandler(ws)
	h.AllowOutsideWorkspace = true
	out, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path": path,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result fileReadBytesResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(result.Content)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if string(decoded) != content {
		t.Errorf("decoded = %q, want %q", string(decoded), content)
	}
}

func TestFileReadBytes_OutsideWorkspace_Blocked_ByDefault(t *testing.T) {
	ws := setupWorkspace(t)
	outside := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(outside)
	path := writeTestFile(t, resolved, "external.bin", "data")

	h := NewFileReadBytesHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path": path,
	}))
	if err == nil {
		t.Fatal("expected error for outside path with default config")
	}
}

// --- FILE_LIST with AllowOutsideWorkspace ---

func TestFileList_OutsideWorkspace_Allowed(t *testing.T) {
	ws := setupWorkspace(t)
	outside := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(outside)
	writeTestFile(t, resolved, "a.txt", "a")
	writeTestFile(t, resolved, "b.txt", "b")

	h := NewFileListHandler(ws)
	h.AllowOutsideWorkspace = true
	out, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path": resolved,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	count := int(result["count"].(float64))
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

func TestFileList_OutsideWorkspace_Blocked_ByDefault(t *testing.T) {
	ws := setupWorkspace(t)
	outside := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(outside)

	h := NewFileListHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path": resolved,
	}))
	if err == nil {
		t.Fatal("expected error for outside path with default config")
	}
}

// --- FILE_SEARCH with AllowOutsideWorkspace ---

func TestFileSearch_OutsideWorkspace_Allowed(t *testing.T) {
	ws := setupWorkspace(t)
	outside := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(outside)
	writeTestFile(t, resolved, "needle.txt", "find this needle\n")

	h := NewFileSearchHandler(ws)
	h.AllowOutsideWorkspace = true
	out, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":  resolved,
		"query": "needle",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	matchCount := int(result["match_count"].(float64))
	if matchCount != 1 {
		t.Errorf("match_count = %d, want 1", matchCount)
	}

	// Verify the match path is absolute (not ../../...) when searching outside workspace.
	matches := result["matches"].([]any)
	match := matches[0].(map[string]any)
	filePath := match["file"].(string)
	if strings.HasPrefix(filePath, "..") {
		t.Errorf("match path should not be relative with .., got %q", filePath)
	}
}

func TestFileSearch_OutsideWorkspace_Blocked_ByDefault(t *testing.T) {
	ws := setupWorkspace(t)
	outside := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(outside)
	writeTestFile(t, resolved, "needle.txt", "find this\n")

	h := NewFileSearchHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":  resolved,
		"query": "find",
	}))
	if err == nil {
		t.Fatal("expected error for outside path with default config")
	}
}

// --- FILE_WRITE and FILE_EDIT stay sandboxed regardless ---

func TestFileWrite_StaysSandboxed(t *testing.T) {
	ws := setupWorkspace(t)
	outside := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(outside)
	target := filepath.Join(resolved, "should-not-exist.txt")

	// FileWriteHandler has no AllowOutsideWorkspace field.
	h := NewFileWriteHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":    target,
		"content": "pwned",
	}))
	if err == nil {
		t.Fatal("expected error: writes must always be sandboxed")
	}

	// Verify nothing was written.
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatal("file was created outside workspace; writes should be blocked")
	}
}

func TestFileEdit_StaysSandboxed(t *testing.T) {
	ws := setupWorkspace(t)
	outside := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(outside)
	path := writeTestFile(t, resolved, "external.txt", "original content")

	// FileEditHandler has no AllowOutsideWorkspace field.
	h := NewFileEditHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":       path,
		"old_string": "original",
		"new_string": "modified",
	}))
	if err == nil {
		t.Fatal("expected error: edits must always be sandboxed")
	}

	// Verify file was not modified.
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "modified") {
		t.Fatal("file was modified outside workspace; edits should be blocked")
	}
}
