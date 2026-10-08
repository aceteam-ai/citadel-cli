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

// TestResolveStateVolumePath_RejectsCitadelConfigEscapeViaSymlink is the
// regression citadel-cli#1163 exists for: ~/.citadel holds the node's own
// config and credentials, so a symlink under the allowed .citadel/instances
// base that points back up at the .citadel config area (or anywhere outside
// instances) must be rejected after the symlink-resolved boundary check -- the
// bind mount would otherwise follow it into the credential area.
func TestResolveStateVolumePath_RejectsCitadelConfigEscapeViaSymlink(t *testing.T) {
	home := t.TempDir()
	instances := filepath.Join(home, ".citadel", "instances")
	if err := os.MkdirAll(instances, 0o755); err != nil {
		t.Fatal(err)
	}
	// Populate the credential area the instance bind must never reach.
	citadelDir := filepath.Join(home, ".citadel")
	if err := os.WriteFile(filepath.Join(citadelDir, "config.yaml"), []byte("node_config_dir: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A symlink inside .citadel/instances pointing UP to the forbidden .citadel
	// config dir. Pre-#1163 (whole-.citadel base) this escape did not even need a
	// symlink; the symlink makes it a lexical-vs-resolved escape that must fail.
	evil := filepath.Join(instances, "escape")
	if err := os.Symlink(citadelDir, evil); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveStateVolumePath("~/.citadel/instances/escape", home); err == nil {
		t.Error("accepted a symlink under .citadel/instances that escapes up into the .citadel config/credential area")
	}

	// A plain (non-symlink) path directly under .citadel but outside instances is
	// rejected too: the whole point is that config/creds are no longer reachable.
	if _, err := resolveStateVolumePath("~/.citadel/config.yaml", home); err == nil {
		t.Error("accepted a bind into the node .citadel config area")
	}

	// A legitimate not-yet-created instance dir under .citadel/instances is still
	// accepted (the state dir is MkdirAll'd later).
	if _, err := resolveStateVolumePath("~/.citadel/instances/i-new", home); err != nil {
		t.Errorf("rejected a legit instance path under .citadel/instances: %v", err)
	}
}
