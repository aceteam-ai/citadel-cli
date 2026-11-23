//go:build !linux && !darwin

package harnessseat

import (
	"errors"
	"os"
	"testing"
)

func TestSaltStoreUnsupportedPlatformDoesNotCreateState(t *testing.T) {
	dir := t.TempDir()
	if s, err := OpenSaltStore(dir); !errors.Is(err, ErrStoreUnsupported) || s != nil {
		t.Fatalf("unsupported privacy boundary accepted: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("unsupported platform created state")
	}
}
