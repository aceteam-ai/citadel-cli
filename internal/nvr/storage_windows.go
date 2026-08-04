//go:build windows

package nvr

import "fmt"

// The nvr module does not run on Windows. Frigate and wyze-bridge are Linux
// containers, and the module additionally requires host networking (TUTK camera
// discovery) plus an NFS/SMB mount owned by the node — none of which Docker
// Desktop's WSL2 backend provides in the shape the module needs.
//
// These stubs exist so the package COMPILES for GOOS=windows (`go build ./...`,
// which is what a contributor runs to check they have not broken Windows) rather
// than failing on the Unix-only syscall.Stat/Statfs in storage_unix.go. They
// return an error rather than a zero value on purpose: a silent `false, nil`
// from isMountpoint would read as "not a mountpoint" and a silent `0, nil` from
// fsType as "not a network filesystem", turning a build error into a runtime
// guard that quietly passes the wrong verdict.
//
// The Windows binary never reaches this code: nothing in ./cmd/citadel imports
// internal/nvr (only ./cmd/nvrconfig, the Linux init container, does).

func isMountpoint(path string) (bool, error) {
	return false, fmt.Errorf("nvr: mountpoint check for %s is not supported on Windows (the nvr module is Linux-only)", path)
}

func fsType(path string) (int64, error) {
	return 0, fmt.Errorf("nvr: filesystem-type check for %s is not supported on Windows (the nvr module is Linux-only)", path)
}
