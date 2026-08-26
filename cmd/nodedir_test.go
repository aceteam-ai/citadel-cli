// cmd/nodedir_test.go
package cmd

import (
	"strings"
	"testing"
)

// setNodeDirOverrideForTest sets the package-level --node-dir flag var for the
// duration of a test and restores it to "" on cleanup, mirroring t.Setenv's
// semantics for a package var rather than an env var (tests in this package
// run sequentially, so a shared var is safe as long as every user restores
// it).
func setNodeDirOverrideForTest(t *testing.T, dir string) {
	t.Helper()
	prev := nodeDirFlag
	nodeDirFlag = dir
	t.Cleanup(func() { nodeDirFlag = prev })
}

func TestResolveNodeDirOverride_FlagWinsOverEnv(t *testing.T) {
	t.Setenv("CITADEL_NODE_DIR", "/from/env")
	setNodeDirOverrideForTest(t, "/from/flag")

	if got := resolveNodeDirOverride(); got != "/from/flag" {
		t.Fatalf("resolveNodeDirOverride() = %q, want the flag value", got)
	}
}

func TestResolveNodeDirOverride_EnvFallback(t *testing.T) {
	t.Setenv("CITADEL_NODE_DIR", "/from/env")
	setNodeDirOverrideForTest(t, "")

	if got := resolveNodeDirOverride(); got != "/from/env" {
		t.Fatalf("resolveNodeDirOverride() = %q, want the env fallback", got)
	}
}

func TestResolveNodeDirOverride_EmptyWhenNeitherSet(t *testing.T) {
	t.Setenv("CITADEL_NODE_DIR", "")
	setNodeDirOverrideForTest(t, "")

	if got := resolveNodeDirOverride(); got != "" {
		t.Fatalf("resolveNodeDirOverride() = %q, want empty (backward-compat default)", got)
	}
}

func TestRefuseIfLockfileWriteUnsupported(t *testing.T) {
	setNodeDirOverrideForTest(t, "")
	if err := refuseIfLockfileWriteUnsupported("citadel module install"); err != nil {
		t.Fatalf("no override active: want nil, got %v", err)
	}

	setNodeDirOverrideForTest(t, "/tmp/some-override")
	err := refuseIfLockfileWriteUnsupported("citadel module install")
	if err == nil {
		t.Fatal("override active: want a refusal error")
	}
	if !strings.Contains(err.Error(), "citadel module install") || !strings.Contains(err.Error(), "--node-dir") {
		t.Fatalf("error = %v, want it to name the command and mention --node-dir", err)
	}
}
