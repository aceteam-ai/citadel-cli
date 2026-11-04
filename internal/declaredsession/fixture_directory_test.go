package declaredsession

import (
	"os"
	"testing"
)

// TempDir follows the caller's umask. Private storage fixtures must pin their
// own metadata rather than accidentally depending on CI's usual umask 0022.
func privateFixtureDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
