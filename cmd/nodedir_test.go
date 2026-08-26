// cmd/nodedir_test.go
package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
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

// --- compose-project scoping under --node-dir (citadel#856 review) ---
//
// These pin the argv contract directly, per the review's instruction: verify
// with Go tests only, never by invoking compose. composeProjectOverride and
// composeArgsWithProject are pure (no exec involved at all), and
// startServiceComposeArgs/stopComposeArgs (cmd/service.go, cmd/stop.go) are
// factored out specifically so the exact argv passed to exec.Command is
// checkable without ever constructing the *exec.Cmd, let alone running it.

func TestComposeProjectOverride_EmptyWithoutOverride(t *testing.T) {
	setNodeDirOverrideForTest(t, "")
	if got := composeProjectOverride(); got != "" {
		t.Fatalf("composeProjectOverride() = %q, want empty (no override -> no -p, preserving #528)", got)
	}
}

func TestComposeProjectOverride_DeterministicPerDir(t *testing.T) {
	dir := t.TempDir()
	setNodeDirOverrideForTest(t, dir)

	first := composeProjectOverride()
	second := composeProjectOverride()
	if first == "" {
		t.Fatal("want a non-empty project name when an override is active")
	}
	if first != second {
		t.Fatalf("composeProjectOverride() must be deterministic for the same dir: got %q then %q", first, second)
	}
	if !strings.HasPrefix(first, "citadel-nodedir-") {
		t.Fatalf("project name = %q, want the citadel-nodedir- prefix", first)
	}
}

func TestComposeProjectOverride_DifferentDirsDifferentProjects(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()

	setNodeDirOverrideForTest(t, dirA)
	projectA := composeProjectOverride()

	setNodeDirOverrideForTest(t, dirB)
	projectB := composeProjectOverride()

	if projectA == projectB {
		t.Fatalf("two different override dirs got the SAME compose project %q -- they would collide", projectA)
	}
}

func TestComposeArgsWithProject_UnchangedWithoutOverride(t *testing.T) {
	setNodeDirOverrideForTest(t, "")
	in := []string{"-f", "/n/services/vllm.yml", "up", "-d"}
	got := composeArgsWithProject(in)
	if strings.Join(got, " ") != strings.Join(in, " ") {
		t.Fatalf("composeArgsWithProject() = %v, want it BYTE-IDENTICAL to input when no override is active (the #528 no-`-p` default)", got)
	}
}

func TestComposeArgsWithProject_PrependsUnderOverride(t *testing.T) {
	setNodeDirOverrideForTest(t, t.TempDir())
	project := composeProjectOverride()

	got := composeArgsWithProject([]string{"-f", "/n/services/vllm.yml", "up", "-d"})
	want := []string{"-p", project, "-f", "/n/services/vllm.yml", "up", "-d"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("composeArgsWithProject() = %v, want %v (-p PROJECT must precede -f, per the compose CLI)", got, want)
	}
}

func TestStartServiceComposeArgs_NoOverride_NoDashP(t *testing.T) {
	setNodeDirOverrideForTest(t, "")
	got := startServiceComposeArgs("/n/services/vllm.yml", "/n/services/vllm.yml")
	for _, a := range got {
		if a == "-p" {
			t.Fatalf("startServiceComposeArgs() = %v, must NOT contain -p when no override is active", got)
		}
	}
	want := []string{"-f", "/n/services/vllm.yml", "up", "-d"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("startServiceComposeArgs() = %v, want %v", got, want)
	}
}

func TestStartServiceComposeArgs_Override_DashPBeforeDashF(t *testing.T) {
	setNodeDirOverrideForTest(t, t.TempDir())
	project := composeProjectOverride()

	got := startServiceComposeArgs("/n/services/vllm.yml", "/n/services/vllm.yml")
	want := []string{"-p", project, "-f", "/n/services/vllm.yml", "up", "-d"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("startServiceComposeArgs() = %v, want %v", got, want)
	}
}

func TestStopComposeArgs_NoOverride_NoDashP(t *testing.T) {
	setNodeDirOverrideForTest(t, "")
	got := stopComposeArgs("/n/services/vllm.yml", false)
	want := []string{"compose", "-f", "/n/services/vllm.yml", "down"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("stopComposeArgs() = %v, want %v (unchanged #528 default)", got, want)
	}
}

func TestStopComposeArgs_Override_DashPBeforeDashF(t *testing.T) {
	setNodeDirOverrideForTest(t, t.TempDir())
	project := composeProjectOverride()

	got := stopComposeArgs("/n/services/vllm.yml", false)
	want := []string{"compose", "-p", project, "-f", "/n/services/vllm.yml", "down"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("stopComposeArgs() = %v, want %v", got, want)
	}
}

func TestStopComposeArgs_Override_RemoveVolumesAfterDown(t *testing.T) {
	setNodeDirOverrideForTest(t, t.TempDir())
	project := composeProjectOverride()

	got := stopComposeArgs("/n/services/vllm.yml", true)
	want := []string{"compose", "-p", project, "-f", "/n/services/vllm.yml", "down", "-v"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("stopComposeArgs() = %v, want %v (-v must stay AFTER down, it's down's own flag)", got, want)
	}
}

func TestComposeCommandFor_ArgvHasProjectUnderOverride(t *testing.T) {
	setNodeDirOverrideForTest(t, t.TempDir())
	project := composeProjectOverride()

	rt := catalog.ContainerRuntime{Bin: "docker", ComposePrefix: []string{"compose"}}
	cmd := composeCommandFor(rt, "-f", "/n/services/vllm.yml", "ps", "--format", "json")

	got := cmd.Args
	want := []string{"docker", "compose", "-p", project, "-f", "/n/services/vllm.yml", "ps", "--format", "json"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("composeCommandFor(...).Args = %v, want %v", got, want)
	}
}

func TestComposeCommandFor_ArgvNoProjectWithoutOverride(t *testing.T) {
	setNodeDirOverrideForTest(t, "")

	rt := catalog.ContainerRuntime{Bin: "docker", ComposePrefix: []string{"compose"}}
	cmd := composeCommandFor(rt, "-f", "/n/services/vllm.yml", "ps", "--format", "json")

	got := cmd.Args
	want := []string{"docker", "compose", "-f", "/n/services/vllm.yml", "ps", "--format", "json"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("composeCommandFor(...).Args = %v, want %v (unchanged #528 default)", got, want)
	}
}

// TestStopServiceByContainer_RefusesUnderOverride pins the OTHER half of the
// fix: the raw (non-compose) container-name fallback has no way to be
// project-scoped at all, so it must refuse outright under an active override
// rather than operate on a bare global "citadel-<name>" that could belong to
// a different node's real container. Deliberately does NOT touch docker --
// the refusal must happen before any exec.Command runs.
func TestStopServiceByContainer_RefusesUnderOverride(t *testing.T) {
	setNodeDirOverrideForTest(t, t.TempDir())

	err := stopServiceByContainer("some-service-not-in-manifest")
	if err == nil {
		t.Fatal("want a refusal error under --node-dir")
	}
	if !strings.Contains(err.Error(), "refusing") || !strings.Contains(err.Error(), "--node-dir") {
		t.Fatalf("error = %v, want it to say it is refusing and mention --node-dir", err)
	}
}

func TestComposeFailureMessage_CrossProjectConflictNamesTheCause(t *testing.T) {
	setNodeDirOverrideForTest(t, t.TempDir())
	raw := `Error response from daemon: Conflict. The container name "/citadel-vllm" is already in use by container "abc123def456". You have to remove (or rename) that container to be able to reuse that name.`

	got := composeFailureMessage("vllm", []byte(raw))
	if !strings.Contains(got, raw) {
		t.Fatalf("composeFailureMessage() should preserve the original docker error, got: %s", got)
	}
	if !strings.Contains(got, "owned by another compose project") {
		t.Fatalf("composeFailureMessage() should name the cause (cross-project container_name conflict), got: %s", got)
	}
	if !strings.Contains(got, "SAFE failure direction") {
		t.Fatalf("composeFailureMessage() should reassure this is the intended safe direction, got: %s", got)
	}
}

func TestComposeFailureMessage_CrossProjectHintOnlyUnderOverride(t *testing.T) {
	setNodeDirOverrideForTest(t, "") // no override
	raw := `Error response from daemon: Conflict. The container name "/citadel-vllm" is already in use by container "abc123def456".`

	got := composeFailureMessage("vllm", []byte(raw))
	if got != raw {
		t.Fatalf("composeFailureMessage() without an override must pass the message through unchanged (the pre-existing #528 conflict path), got: %s", got)
	}
}

// TestRunTUIWorker_RefusesUnderNodeDirOverride pins the second half of the
// config_handler.go fold-in (citadel#856 review): jobs.NewConfigHandler("")
// (internal/worker/handler_adapter.go) is reached from TWO node-worker entry
// points, not one -- `citadel work` (runWork) AND the control center's
// worker mode (runTUIWorker). Both must refuse under an active
// --node-dir/CITADEL_NODE_DIR override, or APPLY_DEVICE_CONFIG could still
// silently land on the real machine's $HOME/citadel-node via the entry point
// that forgot to check. The refusal is the very first statement in
// runTUIWorker, before any file/network access, so this is safe to call
// directly in a test.
func TestRunTUIWorker_RefusesUnderNodeDirOverride(t *testing.T) {
	setNodeDirOverrideForTest(t, t.TempDir())

	err := runTUIWorker(context.Background(), nil)
	if err == nil {
		t.Fatal("want a refusal error when --node-dir/CITADEL_NODE_DIR is active")
	}
	if !strings.Contains(err.Error(), "--node-dir") {
		t.Fatalf("error = %v, want it to mention --node-dir", err)
	}
}
