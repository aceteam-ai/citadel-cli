// cmd/service_test.go
package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	svcports "github.com/aceteam-ai/citadel-cli/services"
)

// effectiveWorkspaceEnv returns the value of the LAST CITADEL_WORKSPACE= entry
// in env (exec.Cmd uses the last duplicate) and whether any entry was present.
func effectiveWorkspaceEnv(env []string) (string, bool) {
	val, found := "", false
	for _, kv := range env {
		if strings.HasPrefix(kv, "CITADEL_WORKSPACE=") {
			val = strings.TrimPrefix(kv, "CITADEL_WORKSPACE=")
			found = true
		}
	}
	return val, found
}

// TestComposeCommandInjectsHostPortEnv is the regression guard for #426: every
// cmd/ compose invocation must carry the citadel-owned host-port vars so compose
// templates that guard their host publish with ${CITADEL_*_HOST_PORT:?...} (#410)
// resolve instead of dying on the :? guard. composeCommand is the single helper
// all such call sites route through, so asserting its env here covers all of them.
func TestComposeCommandInjectsHostPortEnv(t *testing.T) {
	cmd := composeCommand("-f", "/tmp/does-not-matter.yml", "ps", "--format", "json")

	if cmd.Env == nil {
		t.Fatal("composeCommand must set cmd.Env (nil inherits the bare process env, dropping the host-port vars)")
	}

	// Build a set of the command environment for subset membership checks.
	env := make(map[string]struct{}, len(cmd.Env))
	for _, e := range cmd.Env {
		env[e] = struct{}{}
	}

	// Every entry HostPortEnv() emits must be present verbatim. HostPortEnv is
	// static (four managed services), so this is non-vacuous without node config.
	hostPortEnv := svcports.HostPortEnv()
	if len(hostPortEnv) == 0 {
		t.Fatal("HostPortEnv() returned no entries; the assertion below would be vacuous")
	}
	for _, want := range hostPortEnv {
		if _, ok := env[want]; !ok {
			t.Errorf("composeCommand env is missing host-port entry %q; got env:\n%s",
				want, strings.Join(cmd.Env, "\n"))
		}
	}
}

// TestComposeCommandInjectsWorkspaceEnv is the regression guard for #525: every
// cmd/ compose invocation (including read-only ones like `citadel status` ps)
// must carry CITADEL_WORKSPACE so compose files that bind-mount the workspace
// via ${CITADEL_WORKSPACE:?...} (transcribe, meeting) interpolate. Mirrors
// internal/jobs TestComposeEnv_InjectsWorkspace for the CLI/TUI tree.
func TestComposeCommandInjectsWorkspaceEnv(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("CITADEL_WORKSPACE", ws)

	cmd := composeCommand("-f", "/tmp/does-not-matter.yml", "ps", "--format", "json")
	got, found := effectiveWorkspaceEnv(cmd.Env)
	if !found {
		t.Fatal("composeCommand env did not set CITADEL_WORKSPACE")
	}
	if got != ws {
		t.Errorf("CITADEL_WORKSPACE = %q, want the configured workspace %q", got, ws)
	}
}

// TestComposeEnvWorkspaceDefaultsAndNeverEmpty verifies that with no workspace
// configured, composeEnv falls back to the absolute ~/citadel-node/workspace
// default and never leaves an effective empty CITADEL_WORKSPACE= (which would
// still fail the :? guard, or worse, mount the wrong path). Mirrors
// internal/jobs TestComposeEnv_NoWorkspaceLeavesEnvUntouched.
func TestComposeEnvWorkspaceDefaultsAndNeverEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows os.UserHomeDir
	t.Setenv("CITADEL_WORKSPACE", "")

	got, found := effectiveWorkspaceEnv(composeEnv())
	if !found {
		t.Fatal("composeEnv did not set CITADEL_WORKSPACE")
	}
	if got == "" {
		t.Fatal("composeEnv left an effective empty CITADEL_WORKSPACE")
	}
	want := filepath.Join(home, "citadel-node", "workspace")
	if got != want {
		t.Errorf("CITADEL_WORKSPACE = %q, want default %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("CITADEL_WORKSPACE %q is not an absolute path", got)
	}
}
