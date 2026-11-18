package botprofile

import (
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

func TestDenyPathNilResolverAllowsEverything(t *testing.T) {
	withConfigDirs(t)
	ConfigDirs = nil
	if DenyPath("/anything/bot-profiles/x") != nil {
		t.Fatal("nil resolver must protect nothing")
	}
}
