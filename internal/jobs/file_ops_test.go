package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// --- helpers ---

func setupWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Resolve the temp dir itself (macOS /var -> /private/var, etc.)
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolving temp dir: %v", err)
	}
	return resolved
}

func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func makeJob(payload map[string]string) *nexus.Job {
	return &nexus.Job{ID: "test-1", Type: "TEST", Payload: payload}
}

// --- ValidatePath tests ---

func TestValidatePath_BasicAccess(t *testing.T) {
	ws := setupWorkspace(t)
	writeTestFile(t, ws, "hello.txt", "hello")

	got, err := ValidatePath(ws, filepath.Join(ws, "hello.txt"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(got, "hello.txt") {
		t.Errorf("expected path ending in hello.txt, got %q", got)
	}
}

func TestValidatePath_RelativePath(t *testing.T) {
	ws := setupWorkspace(t)
	writeTestFile(t, ws, "sub/file.txt", "data")

	got, err := ValidatePath(ws, "sub/file.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := filepath.Join(ws, "sub", "file.txt")
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestValidatePath_DotDotEscape(t *testing.T) {
	ws := setupWorkspace(t)

	_, err := ValidatePath(ws, filepath.Join(ws, "..", "etc", "passwd"))
	if err == nil {
		t.Fatal("expected error for .. traversal, got nil")
	}
}

func TestValidatePath_AbsoluteOutside(t *testing.T) {
	ws := setupWorkspace(t)

	_, err := ValidatePath(ws, "/etc/passwd")
	if err == nil {
		t.Fatal("expected error for absolute path outside workspace, got nil")
	}
}

func TestValidatePath_SymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks may need admin on Windows")
	}
	ws := setupWorkspace(t)

	// Create a symlink inside the workspace that points outside.
	link := filepath.Join(ws, "escape")
	if err := os.Symlink("/tmp", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := ValidatePath(ws, filepath.Join(ws, "escape", "something"))
	if err == nil {
		t.Fatal("expected error for symlink escape, got nil")
	}
}

func TestValidatePath_NonExistentTarget(t *testing.T) {
	ws := setupWorkspace(t)

	// Path doesn't exist but is within workspace — should succeed.
	got, err := ValidatePath(ws, filepath.Join(ws, "newdir", "newfile.txt"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := filepath.Join(ws, "newdir", "newfile.txt")
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestValidatePath_EmptyInputs(t *testing.T) {
	if _, err := ValidatePath("", "foo"); err == nil {
		t.Error("expected error for empty workspace")
	}
	ws := setupWorkspace(t)
	if _, err := ValidatePath(ws, ""); err == nil {
		t.Error("expected error for empty path")
	}
}

func TestValidatePath_BoundaryPrefix(t *testing.T) {
	// Ensure /workspace doesn't match /workspaceEVIL
	ws := setupWorkspace(t)
	evil := ws + "EVIL"
	if err := os.MkdirAll(evil, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeTestFile(t, evil, "secret.txt", "secret")

	_, err := ValidatePath(ws, filepath.Join(evil, "secret.txt"))
	if err == nil {
		t.Fatal("expected error for boundary prefix attack, got nil")
	}
}

// --- FILE_READ tests ---

func TestFileRead_Basic(t *testing.T) {
	ws := setupWorkspace(t)
	content := "line one\nline two\nline three\n"
	path := writeTestFile(t, ws, "test.txt", content)

	h := NewFileReadHandler(ws)
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
	if result["total_lines"].(float64) != 4 { // trailing newline creates empty 4th line
		t.Errorf("total_lines = %v, want 4", result["total_lines"])
	}
	resultContent := result["content"].(string)
	if !strings.Contains(resultContent, "line one") {
		t.Errorf("content missing 'line one': %s", resultContent)
	}
}

func TestFileRead_OffsetAndLimit(t *testing.T) {
	ws := setupWorkspace(t)
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, strings.Repeat("x", 10))
	}
	path := writeTestFile(t, ws, "big.txt", strings.Join(lines, "\n"))

	h := NewFileReadHandler(ws)
	out, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":   path,
		"offset": "10",
		"limit":  "5",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	json.Unmarshal(out, &result)

	// Should start at line 11 (1-based).
	content := result["content"].(string)
	if !strings.HasPrefix(content, "    11\t") {
		t.Errorf("content should start at line 11, got: %s", content[:40])
	}

	// Should contain exactly 5 lines.
	outputLines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(outputLines) != 5 {
		t.Errorf("expected 5 lines, got %d", len(outputLines))
	}
}

func TestFileRead_BinaryFile(t *testing.T) {
	ws := setupWorkspace(t)
	// Write a file with NUL bytes.
	path := filepath.Join(ws, "binary.dat")
	os.WriteFile(path, []byte{0x89, 0x50, 0x4E, 0x47, 0x00, 0x00, 0x00}, 0644)

	h := NewFileReadHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path": path,
	}))
	if err == nil {
		t.Fatal("expected error for binary file, got nil")
	}
	if !strings.Contains(err.Error(), "binary") {
		t.Errorf("error should mention binary, got: %v", err)
	}
}

func TestFileRead_NonExistent(t *testing.T) {
	ws := setupWorkspace(t)

	h := NewFileReadHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path": filepath.Join(ws, "nope.txt"),
	}))
	if err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
}

func TestFileRead_PathTraversal(t *testing.T) {
	ws := setupWorkspace(t)

	h := NewFileReadHandler(ws)
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

func TestFileRead_MissingPath(t *testing.T) {
	ws := setupWorkspace(t)
	h := NewFileReadHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{}))
	if err == nil {
		t.Fatal("expected error for missing path, got nil")
	}
}

// --- FILE_WRITE tests ---

func TestFileWrite_Basic(t *testing.T) {
	ws := setupWorkspace(t)
	target := filepath.Join(ws, "output.txt")

	h := NewFileWriteHandler(ws)
	out, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":    target,
		"content": "hello world",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	json.Unmarshal(out, &result)
	if result["bytes_written"].(float64) != 11 {
		t.Errorf("bytes_written = %v, want 11", result["bytes_written"])
	}

	data, _ := os.ReadFile(target)
	if string(data) != "hello world" {
		t.Errorf("file content = %q, want %q", string(data), "hello world")
	}
}

func TestFileWrite_CreatesParentDirs(t *testing.T) {
	ws := setupWorkspace(t)
	target := filepath.Join(ws, "a", "b", "c", "deep.txt")

	h := NewFileWriteHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":    target,
		"content": "nested",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(target)
	if string(data) != "nested" {
		t.Errorf("file content = %q, want %q", string(data), "nested")
	}
}

func TestFileWrite_PathTraversal(t *testing.T) {
	ws := setupWorkspace(t)

	h := NewFileWriteHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":    "/tmp/evil-write.txt",
		"content": "pwned",
	}))
	if err == nil {
		t.Fatal("expected error for path outside workspace, got nil")
	}
}

func TestFileWrite_MissingContent(t *testing.T) {
	ws := setupWorkspace(t)
	h := NewFileWriteHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path": filepath.Join(ws, "test.txt"),
	}))
	if err == nil {
		t.Fatal("expected error for missing content, got nil")
	}
}

// --- FILE_EDIT tests ---

func TestFileEdit_SingleReplacement(t *testing.T) {
	ws := setupWorkspace(t)
	path := writeTestFile(t, ws, "edit.txt", "hello world\nfoo bar\n")

	h := NewFileEditHandler(ws)
	out, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":       path,
		"old_string": "foo bar",
		"new_string": "baz qux",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "baz qux") {
		t.Errorf("file should contain 'baz qux', got %q", string(data))
	}
	if strings.Contains(string(data), "foo bar") {
		t.Errorf("file should not contain 'foo bar'")
	}

	var result map[string]any
	json.Unmarshal(out, &result)
	if result["replacements"].(float64) != 1 {
		t.Errorf("replacements = %v, want 1", result["replacements"])
	}
}

func TestFileEdit_NotUnique(t *testing.T) {
	ws := setupWorkspace(t)
	path := writeTestFile(t, ws, "dup.txt", "aaa\naaa\naaa\n")

	h := NewFileEditHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":       path,
		"old_string": "aaa",
		"new_string": "bbb",
	}))
	if err == nil {
		t.Fatal("expected error for non-unique old_string, got nil")
	}
	if !strings.Contains(err.Error(), "3 times") {
		t.Errorf("error should mention count, got: %v", err)
	}
}

func TestFileEdit_ReplaceAll(t *testing.T) {
	ws := setupWorkspace(t)
	path := writeTestFile(t, ws, "all.txt", "aaa\naaa\naaa\n")

	h := NewFileEditHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":        path,
		"old_string":  "aaa",
		"new_string":  "bbb",
		"replace_all": "true",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "aaa") {
		t.Errorf("file should not contain 'aaa' after replace_all")
	}
}

func TestFileEdit_NotFound(t *testing.T) {
	ws := setupWorkspace(t)
	writeTestFile(t, ws, "nope.txt", "hello world")

	h := NewFileEditHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":       filepath.Join(ws, "nope.txt"),
		"old_string": "missing text",
		"new_string": "replacement",
	}))
	if err == nil {
		t.Fatal("expected error for old_string not found, got nil")
	}
}

func TestFileEdit_EmptyOldString(t *testing.T) {
	ws := setupWorkspace(t)
	writeTestFile(t, ws, "empty.txt", "content")

	h := NewFileEditHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":       filepath.Join(ws, "empty.txt"),
		"old_string": "",
		"new_string": "x",
	}))
	if err == nil {
		t.Fatal("expected error for empty old_string, got nil")
	}
}

func TestFileEdit_PathTraversal(t *testing.T) {
	ws := setupWorkspace(t)
	h := NewFileEditHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":       "/etc/passwd",
		"old_string": "root",
		"new_string": "pwned",
	}))
	if err == nil {
		t.Fatal("expected error for path outside workspace, got nil")
	}
}

// --- FILE_LIST tests ---

func TestFileList_Basic(t *testing.T) {
	ws := setupWorkspace(t)
	writeTestFile(t, ws, "a.txt", "a")
	writeTestFile(t, ws, "b.go", "b")
	os.Mkdir(filepath.Join(ws, "subdir"), 0755)

	h := NewFileListHandler(ws)
	out, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path": ws,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	json.Unmarshal(out, &result)
	count := int(result["count"].(float64))
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
}

func TestFileList_WithPattern(t *testing.T) {
	ws := setupWorkspace(t)
	writeTestFile(t, ws, "a.txt", "a")
	writeTestFile(t, ws, "b.go", "b")
	writeTestFile(t, ws, "c.txt", "c")

	h := NewFileListHandler(ws)
	out, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":    ws,
		"pattern": "*.txt",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	json.Unmarshal(out, &result)
	count := int(result["count"].(float64))
	if count != 2 {
		t.Errorf("count = %d, want 2 (*.txt only)", count)
	}
}

func TestFileList_NotDirectory(t *testing.T) {
	ws := setupWorkspace(t)
	path := writeTestFile(t, ws, "file.txt", "data")

	h := NewFileListHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path": path,
	}))
	if err == nil {
		t.Fatal("expected error for non-directory path, got nil")
	}
}

func TestFileList_PathTraversal(t *testing.T) {
	ws := setupWorkspace(t)
	h := NewFileListHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path": "/etc",
	}))
	if err == nil {
		t.Fatal("expected error for path outside workspace, got nil")
	}
}

// --- FILE_SEARCH tests ---

func TestFileSearch_Basic(t *testing.T) {
	ws := setupWorkspace(t)
	writeTestFile(t, ws, "main.go", "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n")
	writeTestFile(t, ws, "readme.txt", "This is a readme\n")

	h := NewFileSearchHandler(ws)
	out, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":  ws,
		"query": "main",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	json.Unmarshal(out, &result)
	matchCount := int(result["match_count"].(float64))
	if matchCount < 2 {
		t.Errorf("match_count = %d, want >= 2 (package main + func main)", matchCount)
	}
}

func TestFileSearch_WithFilePattern(t *testing.T) {
	ws := setupWorkspace(t)
	writeTestFile(t, ws, "code.go", "func hello() {}\n")
	writeTestFile(t, ws, "notes.txt", "func hello\n")

	h := NewFileSearchHandler(ws)
	out, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":         ws,
		"query":        "func",
		"file_pattern": "*.go",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	json.Unmarshal(out, &result)
	matchCount := int(result["match_count"].(float64))
	if matchCount != 1 {
		t.Errorf("match_count = %d, want 1 (only .go file)", matchCount)
	}
}

func TestFileSearch_NoResults(t *testing.T) {
	ws := setupWorkspace(t)
	writeTestFile(t, ws, "file.txt", "hello world\n")

	h := NewFileSearchHandler(ws)
	out, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":  ws,
		"query": "nonexistent_string_xyz",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	json.Unmarshal(out, &result)
	if result["match_count"].(float64) != 0 {
		t.Errorf("match_count = %v, want 0", result["match_count"])
	}
}

func TestFileSearch_SkipsBinaryFiles(t *testing.T) {
	ws := setupWorkspace(t)
	writeTestFile(t, ws, "text.txt", "searchterm in text\n")
	// Write a "binary" file containing the search term but also NUL bytes.
	binPath := filepath.Join(ws, "binary.dat")
	os.WriteFile(binPath, append([]byte("searchterm"), 0x00), 0644)

	h := NewFileSearchHandler(ws)
	out, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":  ws,
		"query": "searchterm",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	json.Unmarshal(out, &result)
	if result["match_count"].(float64) != 1 {
		t.Errorf("match_count = %v, want 1 (binary file should be skipped)", result["match_count"])
	}
}

func TestFileSearch_PathTraversal(t *testing.T) {
	ws := setupWorkspace(t)
	h := NewFileSearchHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":  "/etc",
		"query": "root",
	}))
	if err == nil {
		t.Fatal("expected error for path outside workspace, got nil")
	}
}

func TestFileSearch_MissingQuery(t *testing.T) {
	ws := setupWorkspace(t)
	h := NewFileSearchHandler(ws)
	_, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path": ws,
	}))
	if err == nil {
		t.Fatal("expected error for missing query, got nil")
	}
}

func TestFileSearch_SkipsGitDir(t *testing.T) {
	ws := setupWorkspace(t)
	writeTestFile(t, ws, "real.txt", "target_string\n")
	writeTestFile(t, ws, ".git/objects/pack/data", "target_string\n")

	h := NewFileSearchHandler(ws)
	out, err := h.Execute(JobContext{}, makeJob(map[string]string{
		"path":  ws,
		"query": "target_string",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]any
	json.Unmarshal(out, &result)
	if result["match_count"].(float64) != 1 {
		t.Errorf("match_count = %v, want 1 (.git should be skipped)", result["match_count"])
	}
}

// --- isBinaryContent tests ---

func TestIsBinaryContent(t *testing.T) {
	tests := []struct {
		name   string
		data   []byte
		binary bool
	}{
		{"plain text", []byte("hello world"), false},
		{"empty", []byte{}, false},
		{"nul byte", []byte{0x00}, true},
		{"PNG header", []byte{0x89, 0x50, 0x4E, 0x47, 0x00}, true},
		{"text with newlines", []byte("line1\nline2\r\nline3"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBinaryContent(tt.data); got != tt.binary {
				t.Errorf("isBinaryContent = %v, want %v", got, tt.binary)
			}
		})
	}
}
