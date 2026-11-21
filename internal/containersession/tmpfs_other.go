//go:build !linux

package containersession

import "errors"

// IsTmpfs cannot prove memory-backed storage off Linux, so container sessions
// refuse to materialize credentials there.
func IsTmpfs(string) (bool, error) {
	return false, errors.New("tmpfs check unsupported on this platform")
}
