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

func TestLocalSeatDirectoryUnsupportedDoesNotCreateState(t *testing.T) {
	i := fixtureIdentity(t, Codex)
	for _, create := range []bool{false, true} {
		if dir, err := PrepareLocalSeatDirectory(i, create); !errors.Is(err, ErrStoreUnsupported) || dir != nil {
			t.Fatal("unsupported platform accepted directory preparation")
		}
	}
	entries, err := os.ReadDir(i.HomeDir)
	if err != nil || len(entries) != 0 {
		t.Fatal("unsupported preparation created state")
	}
}
