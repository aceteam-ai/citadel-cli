package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func conditionalPayload(t *testing.T, path, old, replacement string) map[string]string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(old))
	return map[string]string{
		"path": path, "content": replacement,
		"expected_size":        strconv.FormatInt(info.Size(), 10),
		"expected_modified_at": info.ModTime().UTC().Format(time.RFC3339Nano),
		"expected_sha256":      hex.EncodeToString(hash[:]),
	}
}

func TestFileWriteIfUnchanged_SavesAndRejectsStaleVersions(t *testing.T) {
	ws := setupWorkspace(t)
	path := writeTestFile(t, ws, "note.md", "draft")
	handler := &FileWriteIfUnchangedHandler{WorkspaceDir: ws}
	good := conditionalPayload(t, path, "draft", "saved")
	if _, err := handler.Execute(JobContext{}, makeJob(good)); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "saved" {
		t.Fatalf("saved %q", got)
	}
	for name, mutate := range map[string]func(map[string]string){
		"size":   func(p map[string]string) { p["expected_size"] = "4" },
		"mtime":  func(p map[string]string) { p["expected_modified_at"] = "2020-01-01T00:00:00Z" },
		"digest": func(p map[string]string) { p["expected_sha256"] = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			stale := conditionalPayload(t, path, "saved", "overwrite")
			mutate(stale)
			if _, err := handler.Execute(JobContext{}, makeJob(stale)); err == nil || !strings.Contains(err.Error(), "file changed since load") {
				t.Fatalf("expected conflict, got %v", err)
			}
			if got, _ := os.ReadFile(path); string(got) != "saved" {
				t.Fatalf("stale write changed file to %q", got)
			}
		})
	}
}

func TestFileWriteIfUnchanged_EmptyFileAndOutsidePath(t *testing.T) {
	ws := setupWorkspace(t)
	path := writeTestFile(t, ws, "empty.txt", "")
	handler := &FileWriteIfUnchangedHandler{WorkspaceDir: ws}
	if _, err := handler.Execute(JobContext{}, makeJob(conditionalPayload(t, path, "", "new"))); err != nil {
		t.Fatal(err)
	}
	unsafe := conditionalPayload(t, path, "new", "bad")
	unsafe["path"] = t.TempDir() + "/escape.txt"
	if _, err := handler.Execute(JobContext{}, makeJob(unsafe)); err == nil {
		t.Fatal("expected outside-workspace refusal")
	}
}
