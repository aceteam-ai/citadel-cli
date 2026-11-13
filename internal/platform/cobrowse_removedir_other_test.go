//go:build !windows

package platform

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A dying browser helper can recreate files while the session dir is being
// removed; removeSessionDir must keep trying until the dir is really gone.
func TestRemoveSessionDirOutlastsRecreatingWriter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	if err := os.MkdirAll(filepath.Join(dir, "Default"), 0o700); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.WriteFile(filepath.Join(dir, "Default", "f"), []byte("x"), 0o600)
			time.Sleep(time.Millisecond)
		}
	}()
	go func() { time.Sleep(300 * time.Millisecond); close(stop) }()
	removeSessionDirRetry(dir, 50, 50*time.Millisecond)
	wg.Wait()
	// The writer only writes into existing dirs (like a dying child), so it
	// cannot resurrect the tree once removal succeeds.
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("session dir survived removal: %v", err)
	}
}

func TestCobrowseCDPReadyTimeoutEnv(t *testing.T) {
	if got := cobrowseCDPReadyTimeout(); got != 20*time.Second {
		t.Fatalf("default = %v", got)
	}
	t.Setenv("CITADEL_COBROWSE_CDP_WAIT_SECONDS", "90")
	if got := cobrowseCDPReadyTimeout(); got != 90*time.Second {
		t.Fatalf("env = %v", got)
	}
}
