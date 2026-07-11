// internal/compose/envfile.go
//
// Resolution of the install-time config env for service compose invocations.
//
// `citadel service catalog install <name>` writes resolved config values to
// <servicesDir>/<name>.env, next to the installed <name>.yml compose file. But
// docker compose only auto-loads a file literally named ".env" (in the project
// directory), so that sibling is invisible unless passed explicitly with
// --env-file. Any compose file that guards its config with ${VAR:?...}
// interpolation (claudecode's ACETEAM_*/ANTHROPIC_*, livekit's LIVEKIT_*) then
// fails EVERY compose invocation — up, ps, restart, logs — even though the
// config exists on disk. The whatsapp command worked around this locally
// (cmd/whatsapp.go); this helper is the shared fix for the catalog services.
package compose

import (
	"os"
	"path/filepath"
	"strings"
)

// EnvFileArgs returns the "--env-file <path>" arguments for a compose
// invocation against an installed service compose file, when the install-time
// sibling env file (<name>.env next to <name>.yml) exists. Returns nil when
// there is no sibling, so call sites can append unconditionally.
//
// Derive from the ORIGINAL installed compose path, not a platform-stripped
// temp copy — the sibling lives next to the original (same rule as the
// sandbox override).
func EnvFileArgs(composePath string) []string {
	if composePath == "" {
		return nil
	}
	base := strings.TrimSuffix(composePath, filepath.Ext(composePath))
	envPath := base + ".env"
	if fi, err := os.Stat(envPath); err != nil || fi.IsDir() {
		return nil
	}
	return []string{"--env-file", envPath}
}
