//go:build unix

package composerefresh

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestComposeOwnerDecisionWithFakeOwnership(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		uid, gid, wantUID, wantGID uint32
		fail, wantErr              bool
		calls                      int
	}{
		{"same", 1000, 1000, 1000, 1000, false, false, 0},
		{"root replacement", 0, 0, 1000, 1000, false, false, 1},
		{"owner refused", 0, 0, 1000, 1000, true, true, 1},
		{"group refused same user", 1000, 0, 1000, 1000, true, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := composeOwnerDecision(tc.uid, tc.gid, tc.wantUID, tc.wantGID, func() error {
				calls++
				if tc.fail {
					return errors.New("fake refusal")
				}
				return nil
			})
			if (err != nil) != tc.wantErr || calls != tc.calls {
				t.Fatal("ownership decision", err, calls)
			}
		})
	}
}

func TestAtomicComposeAndStampKeepOwnerAndRefuseOwnershipFailure(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"ollama.yml", StampFileName} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			if err := writeCompose(path, "old"); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeCompose(path, "new"); err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			a, b := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
			if a.Uid != b.Uid || a.Gid != b.Gid {
				t.Fatal("owner changed")
			}
			called := false
			err = writeComposeWithOwner(path, "refused", func(file *os.File, existing string) error {
				called = true
				info, statErr := file.Stat()
				if statErr != nil || info.Size() != 0 || existing != path {
					t.Fatal("handoff too late or wrong target")
				}
				return errors.New("fake refusal")
			})
			if err == nil || !called {
				t.Fatal("ownership failure ignored")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != "new" {
				t.Fatal("failed handoff published replacement", err)
			}
		})
	}
}

func TestComposeAndStampWritersUseOwnershipBoundary(t *testing.T) {
	original := composeOwnerHandoff
	t.Cleanup(func() { composeOwnerHandoff = original })
	calls := 0
	composeOwnerHandoff = func(*os.File, string) error { calls++; return errors.New("fake owner refusal") }
	dir := t.TempDir()
	if err := writeCompose(filepath.Join(dir, "ollama.yml"), "refuse"); err == nil {
		t.Fatal("compose writer bypassed ownership")
	}
	if err := writeStamp(dir, managedStamp{Version: "refuse", Hashes: map[string]string{}}); err == nil {
		t.Fatal("stamp writer bypassed ownership")
	}
	if calls != 2 {
		t.Fatal("ownership boundary not reached", calls)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatal("failed ownership left published files", err)
	}
}
