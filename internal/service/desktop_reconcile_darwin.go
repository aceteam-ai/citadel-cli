//go:build darwin

package service

import (
	"fmt"
	"os"
	"path/filepath"
)

// ReconcileBundledDesktopHelper is intentionally callable only from the
// bundled app's sidecar, never from a copied worker or a standalone CLI.
func ReconcileBundledDesktopHelper() (DesktopReconcileStatus, error) {
	if os.Geteuid() == 0 {
		return DesktopStageFailed, fmt.Errorf("desktop helper cannot reconcile as root")
	}
	source, err := os.Executable()
	if err != nil {
		return DesktopStageFailed, err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return DesktopStageFailed, err
	}
	if !bundledDesktopHelper(source) {
		return DesktopStageFailed, fmt.Errorf("desktop helper must run from the app bundle")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return DesktopStageFailed, err
	}
	manager := &launchdManager{}
	return ReconcileDesktopHelper(source, home, func(path string) error {
		return manager.reload(path, true)
	})
}
