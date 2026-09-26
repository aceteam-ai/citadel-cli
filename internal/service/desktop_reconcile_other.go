//go:build !darwin

package service

import "fmt"

func ReconcileBundledDesktopHelper() (DesktopReconcileStatus, error) {
	return DesktopStageFailed, fmt.Errorf("desktop helper reconciliation is available on macOS only")
}
