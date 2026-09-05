// cmd/nodedir_test.go
package cmd

import (
	"context"
	"os"
	"path/filepath"
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

// TestRefuseIfReservationNodeDirUnsupported pins the model-exclusivity
// guard (aceteam#8248/#8249): no override -> nil; the FLAG form active ->
// refuses (internal/jobs only sees CITADEL_NODE_DIR via the environment,
// never the cobra flag); the ENV VAR form active with the identical value
// -> nil, since internal/jobs sees that value directly.
func TestRefuseIfReservationNodeDirUnsupported(t *testing.T) {
	t.Setenv("CITADEL_NODE_DIR", "")
	setNodeDirOverrideForTest(t, "")
	if err := refuseIfReservationNodeDirUnsupported("citadel run --exclusive"); err != nil {
		t.Fatalf("no override active: want nil, got %v", err)
	}

	setNodeDirOverrideForTest(t, "/tmp/some-override")
	err := refuseIfReservationNodeDirUnsupported("citadel run --exclusive")
	if err == nil {
		t.Fatal("flag-form override active: want a refusal error")
	}
	if !strings.Contains(err.Error(), "citadel run --exclusive") || !strings.Contains(err.Error(), "--node-dir") {
		t.Fatalf("error = %v, want it to name the command and mention --node-dir", err)
	}
	setNodeDirOverrideForTest(t, "")

	t.Setenv("CITADEL_NODE_DIR", "/tmp/some-override")
	if err := refuseIfReservationNodeDirUnsupported("citadel run --exclusive"); err != nil {
		t.Fatalf("env-var-form override active: want nil (internal/jobs sees the same value), got %v", err)
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

// TestStopComposeArgs_EnvFilePresent pins the citadel#624 Task-A fix: when a
// sibling <name>.env file exists next to the compose file, `down` must pass it
// via --env-file (a GLOBAL flag, so it precedes `down`). Without it, a compose
// file that hard-requires a var via ${VAR:?} -- the WhatsApp bridge's
// ADMIN_API_KEY -- fails to parse on `down` and the stop/uninstall retries
// forever.
func TestStopComposeArgs_EnvFilePresent(t *testing.T) {
	setNodeDirOverrideForTest(t, "")
	dir := t.TempDir()
	composePath := filepath.Join(dir, "whatsapp-bridge.yml")
	envPath := filepath.Join(dir, "whatsapp-bridge.env")
	if err := os.WriteFile(composePath, []byte("services: {}\n"), 0600); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	if err := os.WriteFile(envPath, []byte("ADMIN_API_KEY=wab_admin_x\n"), 0600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	got := stopComposeArgs(composePath, false)
	want := []string{"compose", "-f", composePath, "--env-file", envPath, "down"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("stopComposeArgs() = %v, want %v (--env-file must precede down when the sibling env exists)", got, want)
	}
}

// TestStopComposeArgs_EnvFileAbsent pins that a service WITHOUT a sibling env
// file gets a byte-identical no-op (the #528 no-`--env-file`/no-`-p` default),
// so the Task-A fix never changes `down` for the common case.
func TestStopComposeArgs_EnvFileAbsent(t *testing.T) {
	setNodeDirOverrideForTest(t, "")
	dir := t.TempDir()
	composePath := filepath.Join(dir, "vllm.yml")
	if err := os.WriteFile(composePath, []byte("services: {}\n"), 0600); err != nil {
		t.Fatalf("write compose: %v", err)
	}

	got := stopComposeArgs(composePath, false)
	want := []string{"compose", "-f", composePath, "down"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("stopComposeArgs() = %v, want %v (no sibling env -> no --env-file)", got, want)
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

// --- embedded container-name namespacing under --node-dir (citadel#860) ---
//
// These pin embeddedContainerName/isEmbeddedService directly (pure, no exec
// involved), matching this issue's "verify with Go tests only" constraint.

func TestIsEmbeddedService(t *testing.T) {
	if !isEmbeddedService("vllm") {
		t.Fatal("vllm is a services.ServiceMap entry, want isEmbeddedService(vllm) = true")
	}
	if isEmbeddedService("some-catalog-module") {
		t.Fatal("a non-ServiceMap name must not be classified as embedded")
	}
}

func TestEmbeddedContainerName_NoOverrideIsUnchanged(t *testing.T) {
	setNodeDirOverrideForTest(t, "")
	if got := embeddedContainerName("vllm"); got != "citadel-vllm" {
		t.Fatalf("embeddedContainerName(vllm) = %q, want citadel-vllm (byte-identical to pre-#860)", got)
	}
}

func TestEmbeddedContainerName_WithOverrideMatchesComposeProjectHash(t *testing.T) {
	setNodeDirOverrideForTest(t, t.TempDir())

	// composeProjectOverride() is "citadel-nodedir-<hash>"; embeddedContainerName
	// must reuse the SAME <hash> -- the whole point of #860 is that the compose
	// project and the container it starts always agree on which override owns
	// them.
	project := composeProjectOverride()
	hash := strings.TrimPrefix(project, "citadel-nodedir-")
	if hash == "" || hash == project {
		t.Fatalf("could not extract hash from project %q", project)
	}

	want := "citadel-" + hash + "-vllm"
	if got := embeddedContainerName("vllm"); got != want {
		t.Fatalf("embeddedContainerName(vllm) = %q, want %q (matching composeProjectOverride's hash)", got, want)
	}
}

func TestEmbeddedContainerName_TwoDifferentOverrideDirsProduceDifferentNames(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()

	setNodeDirOverrideForTest(t, dirA)
	nameA := embeddedContainerName("vllm")

	setNodeDirOverrideForTest(t, dirB)
	nameB := embeddedContainerName("vllm")

	if nameA == nameB {
		t.Fatalf("two different override dirs must materialize DIFFERENT container names for the same service; both = %q", nameA)
	}
}

// --- containerIsRunning's name resolution (citadel#860) ---

func TestResolveModuleContainerName_EmbeddedService_NoOverride(t *testing.T) {
	setNodeDirOverrideForTest(t, "")
	if got := resolveModuleContainerName("vllm"); got != "citadel-vllm" {
		t.Fatalf("resolveModuleContainerName(vllm) = %q, want citadel-vllm", got)
	}
}

func TestResolveModuleContainerName_EmbeddedService_WithOverride(t *testing.T) {
	dir := t.TempDir()
	setNodeDirOverrideForTest(t, dir)
	want := embeddedContainerName("vllm")
	if want == "citadel-vllm" {
		t.Fatal("embeddedContainerName should be override-scoped here, not the plain name -- test setup bug")
	}
	if got := resolveModuleContainerName("vllm"); got != want {
		t.Fatalf("resolveModuleContainerName(vllm) = %q, want %q (must agree with embeddedContainerName)", got, want)
	}
}

// TestResolveModuleContainerName_CatalogModule_UnaffectedByOverride pins the
// non-goal: a name that is NOT a services.ServiceMap entry (a catalog/
// third-party module) always resolves to the plain "citadel-<name>"
// convention, regardless of --node-dir -- those compose files author their
// own container_name and are out of scope for this issue's namespacing.
func TestResolveModuleContainerName_CatalogModule_UnaffectedByOverride(t *testing.T) {
	setNodeDirOverrideForTest(t, t.TempDir())
	if got := resolveModuleContainerName("some-catalog-module"); got != "citadel-some-catalog-module" {
		t.Fatalf("resolveModuleContainerName(catalog module) = %q, want the unnamespaced citadel-<name> convention even under override", got)
	}
}

// --- dryRunContainerNames' override-aware fallback (citadel#860) ---

func TestDryRunContainerNames_FallbackUnnamespacedWithoutOverride(t *testing.T) {
	setNodeDirOverrideForTest(t, "")
	got := dryRunContainerNames(filepath.Join(t.TempDir(), "does-not-exist.yml"), "vllm")
	want := []string{"citadel-vllm"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dryRunContainerNames fallback = %v, want %v", got, want)
	}
}

func TestDryRunContainerNames_FallbackNamespacedUnderOverride(t *testing.T) {
	setNodeDirOverrideForTest(t, t.TempDir())
	got := dryRunContainerNames(filepath.Join(t.TempDir(), "does-not-exist.yml"), "vllm")
	want := []string{embeddedContainerName("vllm")}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dryRunContainerNames fallback under override = %v, want %v", got, want)
	}
}
