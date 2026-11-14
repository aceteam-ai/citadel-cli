//go:build !windows

// internal/platform/cobrowse_runner_other.go
//
// Non-Windows runner selection. Linux keeps the managed-Xvfb runner unchanged.
// macOS also gets it for now (it fails with the existing "Xvfb not found"
// error); a macOS headless runner is a later slice (aceteam#13503 S10).
package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func newDefaultBrowserRunner() browserRunner { return xvfbRunner{} }

// defaultCobrowseSessionsBaseDir is the historical shared parent under the OS
// temp dir; its trust check lives in ensureCobrowseBaseDir.
func defaultCobrowseSessionsBaseDir() string {
	return filepath.Join(os.TempDir(), cobrowseSessionsDirName)
}

// currentProcessStartToken returns 0 off Windows: the Linux sweep verifies the
// recorded PIDs through /proc instead, and the pidfile keeps its three-line form.
func currentProcessStartToken() int64 { return 0 }

// removeSessionDir retries for a bounded time: the browser's helper processes
// (renderer, GPU) can outlive the killed main process for a moment and recreate
// files, so a single RemoveAll can fail with ENOTEMPTY or leave the dir behind.
func removeSessionDir(dir string) error {
	return removeSessionDirRetry(dir, sessionRemoveAttempts, sessionRemoveDelay)
}

// removeSessionDirRetry returns the last error, or one saying the dir is still
// there, when every attempt left it behind.
func removeSessionDirRetry(dir string, attempts int, delay time.Duration) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = os.RemoveAll(dir); err == nil {
			if _, serr := os.Lstat(dir); os.IsNotExist(serr) {
				return nil
			}
			err = fmt.Errorf("%s is still present after removal", dir)
		}
		time.Sleep(delay)
	}
	return err
}

func findChromiumForOS() (string, error) { return findChromiumOnPath() }

// sessionOwnerAlive reports whether a session's recorded owner still runs.
// The start token is a Windows identity check and is unused here.
func sessionOwnerAlive(pid int, _ int64) bool { return pid > 0 && pidAlive(pid) }

// killOrphanSessionProcs kills an interrupted session's browser and Xvfb, each
// only while its recorded identity still verifies.
func killOrphanSessionProcs(browserPID, xvfbPID int) {
	sweepKillIfVerified(browserPID, roleCobrowseBrowser, "browser")
	sweepKillIfVerified(xvfbPID, roleCobrowseXvfb, "Xvfb")
}
