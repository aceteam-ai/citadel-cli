//go:build unix

package containerjobs

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestNewWorkspaceFileIsHandedOffBeforeWriting(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "handoff-fails"}[denied], func(t *testing.T) {
			runtime := newFake(t)
			called := 0
			runtime.fileHandoff = func(file *os.File) error {
				called++
				info, err := file.Stat()
				if err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
					t.Fatal("handoff must precede data writes", err)
				}
				if denied {
					return errors.New("fake ownership refusal")
				}
				return nil
			}
			req := baseRequest()
			req.Path = "new.txt"
			req.ContentB64 = base64.StdEncoding.EncodeToString([]byte("workspace bytes"))
			out, err := (&Engine{EnrolledOrg: fixtureEnrolledOrg, Runtime: runtime}).Run(context.Background(), FileWrite, req)
			if called != 1 {
				t.Fatalf("new file ownership was not handed off: calls=%d", called)
			}
			got, readErr := os.ReadFile(filepath.Join(runtime.root, testVolume, req.Path))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if denied {
				if err == nil || out.Bytes != 0 || len(got) != 0 {
					t.Fatal("ownership failure wrote data or reported success", err, out.Bytes)
				}
			} else if err != nil || string(got) != "workspace bytes" {
				t.Fatal("new file write failed", err)
			}
		})
	}
}

func TestExistingWorkspaceFilePreservesOwnershipAndMode(t *testing.T) {
	runtime := newFake(t)
	path := filepath.Join(runtime.root, testVolume, "existing.txt")
	if err := os.WriteFile(path, []byte("old"), 0640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime.fileHandoff = func(*os.File) error { t.Fatal("existing file must not be chowned"); return nil }
	req := baseRequest()
	req.Path = "existing.txt"
	req.ContentB64 = base64.StdEncoding.EncodeToString([]byte("new"))
	if _, err := (&Engine{EnrolledOrg: fixtureEnrolledOrg, Runtime: runtime}).Run(context.Background(), FileWrite, req); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
		t.Fatal("existing inode/mode changed", err)
	}
	oldOwner := before.Sys().(*syscall.Stat_t)
	newOwner := after.Sys().(*syscall.Stat_t)
	if oldOwner.Uid != newOwner.Uid || oldOwner.Gid != newOwner.Gid {
		t.Fatal("existing owner changed")
	}
}

func TestWriteParentResolutionCannotEscapeVolume(t *testing.T) {
	runtime := newFake(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(runtime.root, testVolume, "escape")); err != nil {
		t.Fatal(err)
	}
	runtime.fileHandoff = func(*os.File) error { t.Fatal("escaped path reached handoff"); return nil }
	req := baseRequest()
	req.Path = "escape/new"
	req.ContentB64 = base64.StdEncoding.EncodeToString([]byte("refuse"))
	if _, err := (&Engine{EnrolledOrg: fixtureEnrolledOrg, Runtime: runtime}).Run(context.Background(), FileWrite, req); err == nil {
		t.Fatal("escaping parent accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
		t.Fatal("outside entry created", err)
	}
}

func TestWorkspaceWriteRefusesHardlinksAndSymlinksBeforeChangingBytes(t *testing.T) {
	for _, kind := range []string{"hard", "symbolic"} {
		t.Run(kind, func(t *testing.T) {
			runtime := newFake(t)
			dir := filepath.Join(runtime.root, testVolume)
			original := filepath.Join(dir, "original")
			alias := filepath.Join(dir, "alias")
			if err := os.WriteFile(original, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			if kind == "hard" {
				err = os.Link(original, alias)
			} else {
				err = os.Symlink("original", alias)
			}
			if err != nil {
				t.Fatal(err)
			}
			runtime.fileHandoff = func(*os.File) error { t.Fatal("existing link must not reach ownership handoff"); return nil }
			req := baseRequest()
			req.Path = "alias"
			req.ContentB64 = base64.StdEncoding.EncodeToString([]byte("overwrite"))
			if _, err := (&Engine{EnrolledOrg: fixtureEnrolledOrg, Runtime: runtime}).Run(context.Background(), FileWrite, req); err == nil {
				t.Fatal("linked write accepted")
			}
			got, err := os.ReadFile(original)
			if err != nil || string(got) != "keep" {
				t.Fatal("linked target modified", err)
			}
		})
	}
}
