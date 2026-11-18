package botprofile

import (
	"os"
	"path/filepath"
	"testing"
)

func withConfigDirs(t *testing.T, dirs ...string) {
	t.Helper()
	old := ConfigDirs
	ConfigDirs = func() []string { return dirs }
	t.Cleanup(func() { ConfigDirs = old })
}

// Runs on every OS: the case-insensitive match is by normalized name, not by
// the host filesystem, so a case-folding filesystem cannot reach the store
// under another spelling.
func TestDenyPathCaseVariantsAndNonexistentStore(t *testing.T) {
	cfg := t.TempDir()
	withConfigDirs(t, cfg)
	for _, name := range []string{"bot-profiles", "BOT-PROFILES", "Bot-Profiles", "bOt-PrOfIlEs"} {
		for _, sub := range []string{"", "node-share.key", "a/b"} {
			if err := DenyPath(filepath.Join(cfg, name, sub)); err == nil {
				t.Errorf("allowed %s/%s", name, sub)
			}
		}
	}
	for _, ok := range []string{"notes.txt", "bot-profiles-not/x", "xbot-profiles/x", "sub/bot-profiles/x"} {
		if err := DenyPath(filepath.Join(cfg, ok)); err != nil {
			t.Errorf("refused control %s: %v", ok, err)
		}
	}
	if !IsStoreName("BOT-profiles") || IsStoreName("bot-profile") {
		t.Fatal("IsStoreName")
	}
}

func TestDenyPathConfigDirThatDoesNotExistYet(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "never-created")
	withConfigDirs(t, cfg)
	if DenyPath(filepath.Join(cfg, "BOT-PROFILES", "x")) == nil {
		t.Fatal("lexical fallback missing")
	}
}

func TestDenyPathNilResolverFailsClosed(t *testing.T) {
	withConfigDirs(t)
	ConfigDirs = nil
	if DenyPath(filepath.Join(t.TempDir(), "anything")) == nil {
		t.Fatal("nil resolver must refuse")
	}
	// An explicit empty resolver means "no protected dirs".
	ConfigDirs = func() []string { return nil }
	if err := DenyPath(filepath.Join(t.TempDir(), "anything")); err != nil {
		t.Fatalf("empty resolver refused: %v", err)
	}
}

// sameFileAncestor is a direct identity check. (A symlink alias is also caught
// by the resolved-path comparison in DenyPath; only a bind mount or a
// case-folded config dir reaches the SameFile branch alone, so this test calls
// the helper directly.)
func TestSameFileAncestorIdentity(t *testing.T) {
	base := t.TempDir()
	cfg := filepath.Join(base, "cfg")
	other := filepath.Join(base, "other")
	for _, d := range []string{cfg, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(cfg, alias); err != nil {
		t.Skip("symlinks unavailable")
	}
	cfgInfo, err := os.Stat(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !sameFileAncestor(cfgInfo, filepath.Join(alias, "BOT-profiles", "x")) {
		t.Fatal("alias of the config dir not recognized by identity")
	}
	if !sameFileAncestor(cfgInfo, filepath.Join(cfg, "bot-profiles")) {
		t.Fatal("direct store not recognized")
	}
	if sameFileAncestor(cfgInfo, filepath.Join(other, "bot-profiles", "x")) {
		t.Fatal("unrelated dir matched")
	}
	if sameFileAncestor(cfgInfo, filepath.Join(alias, "notes", "x")) {
		t.Fatal("non-store name matched")
	}
	// Through DenyPath, with the resolver given the alias spelling and the
	// candidate the real one (and the reverse).
	withConfigDirs(t, alias)
	if DenyPath(filepath.Join(cfg, "bot-profiles", "k")) == nil {
		t.Fatal("DenyPath missed the real spelling")
	}
	withConfigDirs(t, cfg)
	if DenyPath(filepath.Join(alias, "bot-profiles", "k")) == nil {
		t.Fatal("DenyPath missed the alias spelling")
	}
}

func TestDenyPathDanglingSymlinkLeafFailsClosed(t *testing.T) {
	base := t.TempDir()
	withConfigDirs(t, filepath.Join(base, "cfg"))
	link := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "does", "not", "exist"), link); err != nil {
		t.Skip("symlinks unavailable")
	}
	if DenyPath(link) == nil {
		t.Fatal("dangling symlink leaf allowed")
	}
}

func TestDenyPathRefusesPublishTempDir(t *testing.T) {
	cfg := t.TempDir()
	withConfigDirs(t, cfg)
	for _, n := range []string{TempPrefix + "x", ".BOT-PROFILES-TMP-123456"} {
		for _, sub := range []string{"", "node-share.key"} {
			if DenyPath(filepath.Join(cfg, n, sub)) == nil {
				t.Errorf("allowed %s/%s", n, sub)
			}
		}
	}
}
