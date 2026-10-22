//go:build !windows

package worklock

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestProbeContentionNativeFinalPathSubstitution(t *testing.T) {
	// Refuse to enter the FIFO-capable fixture if the production open loses
	// NONBLOCK; a future regression must fail promptly, not wait for a writer.
	source, err := os.ReadFile("probe_contention_unix.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC") {
		t.Fatal("nonblocking native open prerequisite changed")
	}
	for _, kind := range []string{"symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "network")
			path := LockPathForStateDir(stateDir)
			if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			ops := nativeContentionProbeOps()
			open := ops.open
			ops.open = func(path string) (contentionProbeFile, error) {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink" {
					if err := os.Symlink("missing-target", path); err != nil {
						t.Fatal(err)
					}
				} else if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
				return open(path)
			}
			if held, err := probeContention(path, ops); held || err != errContentionProbeUnavailable {
				t.Fatal("final symlink/FIFO replacement did not refuse")
			}
		})
	}
}
