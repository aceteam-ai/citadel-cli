//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package memory

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestTranscriptSummary_RejectsFIFOWithoutBlockingOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hostile.fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	done := make(chan string, 1)
	go func() { done <- TranscriptSummary(path, 100) }()
	select {
	case got := <-done:
		if got != "" {
			t.Fatalf("FIFO produced transcript content: %q", got)
		}
	case <-time.After(500 * time.Millisecond):
		// Opening a FIFO without O_NONBLOCK would hang here until a writer
		// connected, which is exactly what a hostile SessionEnd input can do.
		_ = os.Remove(path)
		t.Fatal("TranscriptSummary blocked opening a FIFO")
	}
}
