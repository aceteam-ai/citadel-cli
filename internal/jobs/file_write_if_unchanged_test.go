package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
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

func TestFileWriteIfUnchanged_CompetingSavesSerializeByFileIdentity(t *testing.T) {
	ws := setupWorkspace(t)
	path := writeTestFile(t, ws, "note.md", "draft")
	secondPath := path
	if runtime.GOOS != "windows" {
		secondPath = filepath.Join(ws, "same-file.md")
		if err := os.Link(path, secondPath); err != nil {
			t.Fatal(err)
		}
	}
	firstContent := "a longer first edit"
	secondContent := "short"
	firstPayload := conditionalPayload(t, path, "draft", firstContent)
	secondPayload := conditionalPayload(t, secondPath, "draft", secondContent)
	firstAtWrite := make(chan struct{})
	secondAtWrite := make(chan struct{})
	firstRelease := make(chan struct{})
	secondRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(firstRelease)
			close(secondRelease)
		})
	}
	defer release()
	firstResult := make(chan error, 1)
	secondResult := make(chan error, 1)
	first := &FileWriteIfUnchangedHandler{WorkspaceDir: ws}
	second := &FileWriteIfUnchangedHandler{WorkspaceDir: ws}
	go func() {
		_, err := first.Execute(JobContext{LogFn: func(string, string) {
			close(firstAtWrite)
			<-firstRelease
		}}, makeJob(firstPayload))
		firstResult <- err
	}()
	select {
	case <-firstAtWrite:
	case err := <-firstResult:
		t.Fatalf("first save failed before its final check: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first save did not reach the write boundary")
	}
	go func() {
		_, err := second.Execute(JobContext{LogFn: func(string, string) {
			close(secondAtWrite)
			<-secondRelease
		}}, makeJob(secondPayload))
		secondResult <- err
	}()
	secondPassedCheck := false
	select {
	case <-secondAtWrite:
		secondPassedCheck = true
	case <-time.After(250 * time.Millisecond):
	}
	release()
	var firstErr, secondErr error
	select {
	case firstErr = <-firstResult:
	case <-time.After(5 * time.Second):
		t.Fatal("first save did not finish")
	}
	select {
	case secondErr = <-secondResult:
	case <-time.After(5 * time.Second):
		t.Fatal("second save did not finish")
	}
	if secondPassedCheck {
		t.Error("second save passed the precondition while the first held the file lock")
	}
	if firstErr != nil || secondErr == nil || !strings.Contains(secondErr.Error(), "file changed since load") {
		t.Fatalf("want one success and one conflict, got first=%v second=%v", firstErr, secondErr)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != firstContent {
		t.Fatalf("winning save is not intact: %q (%v)", got, err)
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

func TestWriteConditionalContentUsesOpenedDescriptorAfterPathSwap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("open-file rename and symlink behavior differs on Windows")
	}
	ws := setupWorkspace(t)
	path := writeTestFile(t, ws, "note.md", "draft")
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Rename(path, path+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if err := writeConditionalContent(file, []byte("saved")); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "secret" {
		t.Fatalf("outside file changed: %q %v", got, err)
	}
	if got, err := os.ReadFile(path + ".moved"); err != nil || string(got) != "saved" {
		t.Fatalf("opened file was not saved: %q %v", got, err)
	}
}
