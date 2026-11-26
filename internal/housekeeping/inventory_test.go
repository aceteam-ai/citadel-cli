package housekeeping

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

func openFixtureRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	return root
}

func TestInspectTreeIncludesDirectoryTimesAndPreservesUnknowns(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "fixture")
	if err := os.WriteFile(name, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := old.Add(time.Hour)
	if err := os.Chtimes(name, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, newer, newer); err != nil {
		t.Fatal(err)
	}
	root := openFixtureRoot(t, dir)
	got, err := InspectTree(context.Background(), root, TreeLimits{MaxEntries: 1, MaxDepth: 1})
	if err != nil || !got.NewestEntryAt.Equal(newer) {
		t.Fatalf("directory time lost: %+v %v", got, err)
	}
	now := newer.Add(8 * 24 * time.Hour)
	o := got.Observation(now)
	plan, err := Plan(now, Input{Version: 1, Targets: []Target{{ID: "scratch-a", Kind: "scratch", Observation: &o}}})
	if err != nil || plan.Targets[0].Reason != "free_space_unknown" || plan.EstimatedCandidateBytes != 0 || plan.FreedBytes != 0 || plan.DeletionEnabled {
		t.Fatalf("inventory authorized cleanup: %+v %v", plan, err)
	}
	free := uint64(0)
	o.AvailableBytes = &free
	plan, err = Plan(now, Input{Version: 1, Targets: []Target{{ID: "scratch-a", Kind: "scratch", Observation: &o}}})
	if err != nil || plan.Targets[0].Reason != "open_handles_unknown" || plan.Targets[0].Decision != "skip" {
		t.Fatalf("free space invented handle safety: %+v %v", plan, err)
	}
	encoded, err := json.Marshal(got)
	if err != nil || strings.Contains(string(encoded), "fixture") || strings.Contains(string(encoded), dir) {
		t.Fatal("inventory retained entry names")
	}
	if _, err := root.Stat("fixture"); err != nil {
		t.Fatal("inspector closed caller root", err)
	}
}

func TestInspectTreeBatchesAndExactEntryLimit(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 130; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("item-%03d", i)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root := openFixtureRoot(t, dir)
	got, err := InspectTree(context.Background(), root, TreeLimits{MaxEntries: 130, MaxDepth: 1})
	if err != nil || got.Entries != 130 || got.LogicalBytes != 130 {
		t.Fatalf("batch truncated: %+v %v", got, err)
	}
	got, err = InspectTree(context.Background(), root, TreeLimits{MaxEntries: 129, MaxDepth: 1})
	if err != ErrTreeLimit || got != (TreeInventory{}) {
		t.Fatal("partial batch accepted", got, err)
	}
}

func TestInspectTreeEmptyDepthAndInvalidInput(t *testing.T) {
	dir := t.TempDir()
	root := openFixtureRoot(t, dir)
	ctx := context.Background()
	good := TreeLimits{MaxEntries: 8, MaxDepth: 1}
	got, err := InspectTree(ctx, root, good)
	if err != nil || got.Entries != 0 || got.NewestEntryAt.IsZero() {
		t.Fatal("empty inventory invalid", got, err)
	}
	for _, limits := range []TreeLimits{{}, {MaxEntries: -1, MaxDepth: 1}, {MaxEntries: 1_000_001, MaxDepth: 1}, {MaxEntries: 1, MaxDepth: 0}, {MaxEntries: 1, MaxDepth: 65}} {
		if got, err := InspectTree(ctx, root, limits); err != ErrTreeInput || got != (TreeInventory{}) {
			t.Fatal("invalid limits accepted", got, err)
		}
	}
	if _, err := InspectTree(nil, root, good); err != ErrTreeInput {
		t.Fatal("nil context accepted")
	}
	if _, err := InspectTree(ctx, nil, good); err != ErrTreeInput {
		t.Fatal("nil root accepted")
	}
	if err := os.Mkdir(filepath.Join(dir, "one"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectTree(ctx, root, good); err != nil {
		t.Fatal("exact depth refused", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "one", "two"), 0700); err != nil {
		t.Fatal(err)
	}
	if got, err := InspectTree(ctx, root, good); err != ErrTreeLimit || got != (TreeInventory{}) {
		t.Fatal("depth overflow accepted", got, err)
	}
}

func TestInspectTreeCanceledAndClosedRootsReturnFiniteErrors(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	limits := TreeLimits{MaxEntries: 8, MaxDepth: 1}
	if got, err := InspectTree(ctx, root, limits); err != ErrTreeCanceled || got != (TreeInventory{}) {
		t.Fatal("canceled work accepted", got, err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := InspectTree(context.Background(), root, limits)
	if err != ErrTreeRead || got != (TreeInventory{}) || strings.Contains(fmt.Sprint(err), dir) {
		t.Fatal("closed root error exposed input", got, err)
	}
}

func TestInspectTreeRefusesObservedSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture symlinks require operator privileges")
	}
	for _, targetKind := range []string{"file", "directory", "outside", "missing"} {
		t.Run(targetKind, func(t *testing.T) {
			dir := t.TempDir()
			target := "fixture"
			switch targetKind {
			case "file":
				if err := os.WriteFile(filepath.Join(dir, target), []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(filepath.Join(dir, target), 0700); err != nil {
					t.Fatal(err)
				}
			case "outside":
				target = t.TempDir()
			}
			if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
				t.Fatal(err)
			}
			root := openFixtureRoot(t, dir)
			got, err := InspectTree(context.Background(), root, TreeLimits{MaxEntries: 8, MaxDepth: 4})
			if err != ErrTreeUnsupported || got != (TreeInventory{}) {
				t.Fatal("observed symlink accepted", got, err)
			}
		})
	}
}

type cancelTreeAfterChecks struct {
	context.Context
	remaining int
}

func (c *cancelTreeAfterChecks) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestInspectTreeMidWalkCancellationDiscardsAllMetadata(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root := openFixtureRoot(t, dir)
	ctx := &cancelTreeAfterChecks{Context: context.Background(), remaining: 5}
	got, err := InspectTree(ctx, root, TreeLimits{MaxEntries: 8, MaxDepth: 4})
	if err != ErrTreeCanceled || got != (TreeInventory{}) {
		t.Fatal("canceled partial inventory escaped", got, err)
	}
}

func TestInspectTreeConcurrentInspectionsDoNotShareCounts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fixture"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	root := openFixtureRoot(t, dir)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := InspectTree(context.Background(), root, TreeLimits{MaxEntries: 1, MaxDepth: 1})
			if err != nil || got.Entries != 1 || got.RegularFiles != 1 || got.LogicalBytes != 7 {
				t.Errorf("shared result state: %+v %v", got, err)
			}
		}()
	}
	wg.Wait()
}
