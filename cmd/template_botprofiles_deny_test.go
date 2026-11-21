package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/botprofile"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

// A template input that names a file in a bot-profile store (workspace holds
// the node config dir) is refused before it is opened or staged.
func TestTemplateInputRefusesBotProfileStore(t *testing.T) {
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	cfg := filepath.Join(ws, "citadel-node")
	store := botprofile.Dir(cfg)
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "node-share.key"), []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := botprofile.ConfigDirs
	botprofile.ConfigDirs = func() []string { return []string{cfg} }
	t.Cleanup(func() { botprofile.ConfigDirs = old })

	root, err := os.OpenRoot(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.Mkdir("staged", 0o700); err != nil {
		t.Fatal(err)
	}
	staged, err := root.OpenRoot("staged")
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	files := []jobs.TemplateInputFile{{Path: "x", NodeID: "n1", NodePath: "citadel-node/bot-profiles/node-share.key"}}
	_, _, _, err = (liveTemplateRunOps{nodeID: "n1"}).resolveInputs(context.Background(), files, root, staged, filepath.Join(ws, "staged"), 1<<20)
	if err == nil {
		t.Fatal("template input read the node share")
	}
	if ents, _ := os.ReadDir(filepath.Join(ws, "staged")); len(ents) != 0 {
		t.Fatalf("something was staged: %v", ents)
	}
}
