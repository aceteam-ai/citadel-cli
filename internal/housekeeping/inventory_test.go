package housekeeping

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInspectTreeCountsMetadataWithoutCleanupAuthority(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "fixture"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	got, err := InspectTree(context.Background(), root, TreeLimits{MaxEntries: 8, MaxDepth: 4})
	if err != nil {
		t.Fatal(err)
	}
	if got.Entries != 2 || got.RegularFiles != 1 || got.LogicalBytes != 7 || got.NewestEntryAt.IsZero() {
		t.Fatalf("unexpected inventory: %+v", got)
	}
	o := got.Observation(time.Now())
	if o.OpenHandles != Unknown || o.InUse != Unknown || o.WorktreeClean != Unknown || o.FullyPushed != Unknown || o.AvailableBytes != nil {
		t.Fatalf("metadata invented safety: %+v", o)
	}
	if _, err := os.Stat(filepath.Join(dir, "nested", "fixture")); err != nil {
		t.Fatal("fixture changed", err)
	}
}

func TestInspectTreeRefusesPartialInventory(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	got, err := InspectTree(context.Background(), root, TreeLimits{MaxEntries: 1, MaxDepth: 4})
	if err != ErrTreeLimit || got != (TreeInventory{}) {
		t.Fatalf("partial result escaped: %+v %v", got, err)
	}
}
