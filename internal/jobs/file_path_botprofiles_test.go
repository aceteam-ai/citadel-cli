package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/botprofile"
)

// Bot profile stores are denied to every FILE_* sandbox variant, including the
// relaxed allowOutside mode, authorized roots, and a symlink into the store.
func TestBotProfilesDeniedToFileSandboxes(t *testing.T) {
	cfg := t.TempDir()
	store := botprofile.Dir(cfg)
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(store, "bot-x.enc")
	if err := os.WriteFile(blob, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := botprofile.ConfigDirs
	botprofile.ConfigDirs = func() []string { return []string{cfg} }
	t.Cleanup(func() { botprofile.ConfigDirs = old })

	// The workspace IS the node config dir: the worst case for a lexical check.
	for _, target := range []string{blob, store, filepath.Join(store, "new.enc"), filepath.Join("bot-profiles", "bot-x.enc")} {
		if _, err := ValidatePath(cfg, target); err == nil {
			t.Errorf("ValidatePath allowed %s", target)
		}
		if _, err := ValidateReadPath(cfg, target, false); err == nil {
			t.Errorf("ValidateReadPath allowed %s", target)
		}
		if _, err := ValidateReadPath(cfg, target, true); err == nil {
			t.Errorf("ValidateReadPath(outside) allowed %s", target)
		}
		if _, err := ValidateReadPathWithRoots("", []string{cfg}, abs(cfg, target), false); err == nil {
			t.Errorf("roots allowed %s", target)
		}
		if _, err := ValidateWithinRoots([]string{cfg}, abs(cfg, target)); err == nil {
			t.Errorf("ValidateWithinRoots allowed %s", target)
		}
	}

	// A symlink in the workspace pointing into the store is refused too.
	ws := t.TempDir()
	link := filepath.Join(ws, "innocent")
	if err := os.Symlink(blob, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := ValidateReadPath(ws, link, true); err == nil {
		t.Error("symlink into store allowed (relaxed)")
	}

	// Control: a sibling path in the same workspace still validates.
	other := filepath.Join(cfg, "notes.txt")
	if err := os.WriteFile(other, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidatePath(cfg, other); err != nil {
		t.Errorf("control path refused: %v", err)
	}
	// A name that merely starts with the store name is not the store.
	near := filepath.Join(cfg, "bot-profiles-not")
	if _, err := ValidatePath(cfg, near); err != nil {
		t.Errorf("lookalike refused: %v", err)
	}
}

func abs(base, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

// A directory walk from a permitted parent must not descend into the store.
func TestBotProfilesSkippedByWalkAndList(t *testing.T) {
	cfg := t.TempDir()
	store := botprofile.Dir(cfg)
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "bot-x.enc"), []byte("needle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "plain.txt"), []byte("needle"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := botprofile.ConfigDirs
	botprofile.ConfigDirs = func() []string { return []string{cfg} }
	t.Cleanup(func() { botprofile.ConfigDirs = old })

	entries, _ := os.ReadDir(cfg)
	for _, e := range entries {
		if e.Name() == botprofile.DirName && !isBotProfilesDir(filepath.Join(cfg, e.Name()), e) {
			t.Fatal("store dir entry not recognized")
		}
		if e.Name() == "plain.txt" && isBotProfilesDir(filepath.Join(cfg, e.Name()), e) {
			t.Fatal("plain file flagged")
		}
	}
}

// FILE_SEARCH and FILE_READ, the job handlers themselves, refuse the store, and
// a search rooted at the config dir never reports a hit inside it.
func TestBotProfilesDeniedThroughFileHandlers(t *testing.T) {
	cfg, _ := filepath.EvalSymlinks(t.TempDir())
	store := botprofile.Dir(cfg)
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "bot-x.enc"), []byte("needle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "plain.txt"), []byte("needle"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := botprofile.ConfigDirs
	botprofile.ConfigDirs = func() []string { return []string{cfg} }
	t.Cleanup(func() { botprofile.ConfigDirs = old })

	h := NewFileSearchHandler(cfg)
	if _, err := h.Execute(JobContext{}, makeJob(map[string]string{"path": store, "query": "needle"})); err == nil {
		t.Fatal("FILE_SEARCH accepted the store as its root")
	}
	out, err := h.Execute(JobContext{}, makeJob(map[string]string{"path": cfg, "query": "needle"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "plain.txt") || strings.Contains(string(out), "bot-x") {
		t.Fatalf("search output wrong: %s", out)
	}

	r := NewFileReadHandler(cfg)
	r.AllowOutsideWorkspace = true
	if _, err := r.Execute(JobContext{}, makeJob(map[string]string{"path": filepath.Join(store, "bot-x.enc")})); err == nil {
		t.Fatal("FILE_READ read a sealed blob")
	}
}
