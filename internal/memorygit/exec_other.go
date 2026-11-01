//go:build !linux

package memorygit

import (
	"context"
	"os"
	"path/filepath"
)

func validAbsolutePath(path string) bool { return filepath.IsAbs(path) && filepath.Clean(path) == path }
func openRepository(context.Context, string) (*os.File, error) {
	return nil, failure("unsupported_platform")
}
func runGit(context.Context, string, *os.File, ...string) ([]byte, error) {
	return nil, failure("unsupported_platform")
}
