// cmd/service_test.go
package cmd

import (
	"strings"
	"testing"

	svcports "github.com/aceteam-ai/citadel-cli/services"
)

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
