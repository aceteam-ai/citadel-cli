package cmd

import (
	"os"
	"testing"
)

// privateTempDir is t.TempDir with mode 0700. The hpc profile refuses a state
// base that group or other can write without the sticky bit, and a developer
// umask of 002 makes t.TempDir 0775. This is explicit per fixture, not a
// process-wide umask, which would change modes other tests assert.
func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
