//go:build linux || darwin

package housekeeping

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
)

func TestInspectTreeRefusesSpecialEntryWithoutOpeningIt(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "fixture-pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	root := openFixtureRoot(t, dir)
	got, err := InspectTree(context.Background(), root, TreeLimits{MaxEntries: 8, MaxDepth: 4})
	if err != ErrTreeUnsupported || got != (TreeInventory{}) {
		t.Fatal("special entry accepted", got, err)
	}
}
