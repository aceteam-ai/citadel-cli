//go:build !windows && !darwin

// internal/platform/cobrowse_runner_other.go
//
// Linux (and other non-Windows, non-macOS) runner selection: the managed-Xvfb
// runner, unchanged. Windows and macOS have their own headless runners
// (cobrowse_runner_windows.go, cobrowse_runner_darwin.go).
package platform

import (
	"os"
	"path/filepath"
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

func findChromiumForOS() (string, error) { return findChromiumOnPath() }

// sessionOwnerAlive reports whether a session's recorded owner still runs.
// The start token is a Windows identity check and is unused here.
func sessionOwnerAlive(pid int, _ int64) bool { return pid > 0 && pidAlive(pid) }

// killOrphanSessionProcs kills an interrupted session's browser and Xvfb, each
// only while its recorded identity still verifies.
func killOrphanSessionProcs(_ string, browserPID, xvfbPID int) error {
	sweepKillIfVerified(browserPID, roleCobrowseBrowser, "browser")
	sweepKillIfVerified(xvfbPID, roleCobrowseXvfb, "Xvfb")
	return nil
}
