//go:build !windows

package update

import "errors"

// ExecutableOwnership is only implemented on Windows.
func ExecutableOwnership(path string) (OwnerInfo, error) {
	return OwnerInfo{}, errors.New("executable ownership is only checked on Windows")
}
