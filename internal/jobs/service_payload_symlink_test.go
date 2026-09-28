package jobs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveStateVolumePath_RejectsSymlinkEscape is the exact regression the A1
// review found: a symlink placed INSIDE an allowed base that points OUTSIDE it
// passed the old lexical prefix check, so the bind mount would follow it out of
// the citadel data area. The symlink-resolved boundary check must reject it.
func TestResolveStateVolumePath_RejectsSymlinkEscape(t *testing.T) {
	home := t.TempDir()
	cache := filepath.Join(home, "citadel-cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir() // a directory OUTSIDE the citadel data area

	// A symlink inside citadel-cache pointing OUT of it.
	evil := filepath.Join(cache, "evil")
	if err := os.Symlink(outside, evil); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveStateVolumePath("~/citadel-cache/evil", home); err == nil {
		t.Error("accepted a symlink inside citadel-cache that escapes to an outside dir")
	}

	// A symlink inside citadel-cache pointing to a sibling INSIDE it is fine.
	realInside := filepath.Join(cache, "real")
	if err := os.MkdirAll(realInside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realInside, filepath.Join(cache, "oklink")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveStateVolumePath("~/citadel-cache/oklink", home); err != nil {
		t.Errorf("rejected a legit in-cache symlink: %v", err)
	}

	// A not-yet-created path inside the base is accepted (the state dir is made
	// later by MkdirAll), so the check must not require the leaf to exist.
	if _, err := resolveStateVolumePath("~/citadel-cache/instances/newone", home); err != nil {
		t.Errorf("rejected a not-yet-existent in-base path: %v", err)
	}
}
