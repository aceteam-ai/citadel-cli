package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// Local mode keeps its state under the invoking user's config dir (#1592).
func TestLocalStateDirIsPerUser(t *testing.T) {
	got := localStateDir()
	if want := filepath.Join(platform.ConfigDir(), localStateDirName); got != want {
		t.Fatalf("localStateDir() = %q, want %q", got, want)
	}
}

// Every local-mode entry point resolves co-browse state through localStateDir,
// never the machine-convergent node config dir, which belongs to the node's
// account on an enrolled computer (#1592). Only the doctor's passphrase-vault
// check may read the node dir.
func TestLocalModeNeverUsesNodeConfigDirForState(t *testing.T) {
	for _, f := range []string{"local.go", "local_audit.go", "local_doctor.go", "mcp_local_cobrowse.go", "mcp.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := string(src)
		if regexp.MustCompile(`ownerDir:\s*network\.GetNodeConfigDir`).MatchString(s) {
			t.Errorf("%s wires the co-browse ownerDir to the node config dir", f)
		}
		if f == "local_doctor.go" {
			if regexp.MustCompile(`(AcquireOwner|localaudit\.Verify)\([^)]*nodeDir`).MatchString(s) {
				t.Errorf("%s checks the owner lock or audit log in the node config dir", f)
			}
			continue
		}
		if f != "mcp.go" && strings.Contains(s, "network.GetNodeConfigDir") {
			t.Errorf("%s reads network.GetNodeConfigDir", f)
		}
	}
	for _, f := range []string{"local.go", "mcp.go"} {
		src, _ := os.ReadFile(f)
		if !strings.Contains(string(src), "ownerDir: localStateDir") {
			t.Errorf("%s does not wire ownerDir to localStateDir", f)
		}
	}
}

// A second OS user works even when the node config dir cannot be created by
// them: the owner lock lives in their own state dir.
func TestLocalOwnerLockIgnoresUnusableNodeDir(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := localcobrowse.AcquireOwner(filepath.Join(blocker, "citadel-node")); err == nil {
		t.Fatal("expected the unusable node dir to fail, as on the #1592 machine")
	}
	state := t.TempDir()
	old := localStateDir
	localStateDir = func() string { return filepath.Join(state, localStateDirName) }
	t.Cleanup(func() { localStateDir = old })
	o, err := localcobrowse.AcquireOwner(localStateDir())
	if err != nil {
		t.Fatalf("owner lock in the per-user state dir: %v", err)
	}
	o.Release()
}
