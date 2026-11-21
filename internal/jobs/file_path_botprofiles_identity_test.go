package jobs

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/botprofile"
)

func protectConfigDir(t *testing.T, cfg string) {
	t.Helper()
	old := botprofile.ConfigDirs
	botprofile.ConfigDirs = func() []string { return []string{cfg} }
	t.Cleanup(func() { botprofile.ConfigDirs = old })
}

// The workspace contains the node config dir and the store does not exist yet:
// FILE_FETCH must refuse a destination inside it, in any case spelling, and
// must not create the directory.
func TestFileFetchRefusesBotProfilesDestination(t *testing.T) {
	for _, dest := range []string{
		"citadel-node/bot-profiles/node-share.key",
		"citadel-node/BOT-Profiles/node-share.key",
	} {
		h, job := fileFetchFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("denied destination reached HTTP") })
		cfg := filepath.Join(h.WorkspaceDir, "citadel-node")
		if err := os.MkdirAll(cfg, 0o700); err != nil {
			t.Fatal(err)
		}
		protectConfigDir(t, cfg)
		job.Payload["dest_path"] = dest
		if _, err := h.Execute(JobContext{}, job); err == nil || !strings.Contains(err.Error(), "invalid_dest_path") {
			t.Fatalf("%s: want invalid_dest_path, got %v", dest, err)
		}
		if ents, _ := os.ReadDir(cfg); len(ents) != 0 {
			t.Fatalf("%s: config dir was modified: %v", dest, ents)
		}
	}
}

// A path reaching a non-existent store through a symlinked config dir is
// refused too.
func TestBotProfilesNonexistentStoreThroughSymlinkedConfigDir(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	cfg := filepath.Join(base, "cfg")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cfg, filepath.Join(ws, "alias")); err != nil {
		t.Skip("symlinks unavailable")
	}
	protectConfigDir(t, cfg)
	for _, p := range []string{"alias/bot-profiles/node-share.key", "alias/BOT-PROFILES/x", "alias/bot-profiles"} {
		if _, err := ValidatePath(ws, p); err == nil {
			t.Errorf("ValidatePath allowed %s", p)
		}
	}
}

func seedStoreAndLeakLink(t *testing.T) string {
	t.Helper()
	cfg, _ := filepath.EvalSymlinks(t.TempDir())
	store := botprofile.Dir(cfg)
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "node-share.key"), []byte("needle share material"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "plain.txt"), []byte("needle plain"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(store, "node-share.key"), filepath.Join(cfg, "innocent.txt")); err != nil {
		t.Skip("symlinks unavailable")
	}
	protectConfigDir(t, cfg)
	return cfg
}

func TestFileSearchDoesNotFollowSymlinkIntoStore(t *testing.T) {
	cfg := seedStoreAndLeakLink(t)
	out, err := NewFileSearchHandler(cfg).Execute(JobContext{}, makeJob(map[string]string{"path": cfg, "query": "needle"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "plain.txt") || strings.Contains(string(out), "innocent") || strings.Contains(string(out), "share") {
		t.Fatalf("search leaked the store: %s", out)
	}
}

func TestFileIndexDoesNotFollowSymlinkIntoStore(t *testing.T) {
	tei := fakeTEI(t)
	t.Setenv("CITADEL_TEI_URL", tei.URL)
	t.Setenv("CITADEL_INDEX_DB", "")
	cfg := seedStoreAndLeakLink(t)
	h := NewFileIndexHandler(cfg, filepath.Join(t.TempDir(), "i.db"))
	out, err := h.Execute(JobContext{}, makeJob(map[string]string{"path": cfg}))
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res["files_indexed"].(float64) != 1 {
		t.Fatalf("only plain.txt may be indexed, got %s", out)
	}
}

func TestFileListHidesStoreAndLinksIntoIt(t *testing.T) {
	cfg := seedStoreAndLeakLink(t)
	out, err := NewFileListHandler(cfg).Execute(JobContext{}, makeJob(map[string]string{"path": cfg}))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "plain.txt") || strings.Contains(s, "innocent") || strings.Contains(s, "bot-profiles") {
		t.Fatalf("list exposes the store: %s", s)
	}
}

// The publish temp dir holds the node share while it exists and is denied to
// file jobs and hidden from listings like the store itself.
func TestPublishTempDirDeniedToFileJobs(t *testing.T) {
	cfg, _ := filepath.EvalSymlinks(t.TempDir())
	tmp := filepath.Join(cfg, botprofile.TempPrefix+"abc123")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	share := filepath.Join(tmp, "node-share.key")
	if err := os.WriteFile(share, []byte("needle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "plain.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	protectConfigDir(t, cfg)
	if _, err := ValidateReadPath(cfg, share, true); err == nil {
		t.Error("relaxed read allowed the temp share")
	}
	if _, err := ValidatePath(cfg, share); err == nil {
		t.Error("ValidatePath allowed the temp share")
	}
	out, err := NewFileListHandler(cfg).Execute(JobContext{}, makeJob(map[string]string{"path": cfg}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "bot-profiles-tmp") || !strings.Contains(string(out), "plain.txt") {
		t.Fatalf("listing wrong: %s", out)
	}
}
