// internal/compose/legacyproject.go
//
// Transitional cleanup for the compose project-name unification (citadel #528).
//
// Historically three divergent compose project-name regimes existed: most paths
// (boot, `citadel run`/`stop`, SERVICE_START/STOP jobs, all status reads) passed
// no -p (default project = compose dir basename, i.e. "services"), while the TUI
// service actions, APPLY_DEVICE_CONFIG startServices, and `module update` passed
// `-p citadel-<name>`. Production containers therefore live under the default
// project, and the minority `-p citadel-<name>` invocations silently matched
// nothing (the TUI stop no-op). The fix standardizes on NO -p everywhere.
//
// The transitional hazard: a container previously started WITH `-p
// citadel-<name>` carries a com.docker.compose.project=citadel-<name> label, so
// (a) a no-p `docker compose down` cannot see it, and (b) a no-p `docker compose
// up` conflicts on the pinned container_name ("citadel-<name>") because the
// existing container belongs to another compose project (--force-recreate does
// NOT resolve a cross-project name conflict; compose refuses to adopt it).
// RemoveLegacyProjectContainers force-removes every container labeled with the
// legacy per-service project so both up and down converge under the default
// project. Call it before a compose up and after a compose down.
package compose

import (
	"os/exec"
	"strings"
)

// RemoveLegacyProjectContainers force-removes all containers created under the
// legacy per-service compose project "citadel-<serviceName>". engineBin is the
// container engine binary ("docker" or "podman"). Best-effort by design: it
// returns the removed container IDs for logging, and swallows engine errors (a
// missing engine or no matching containers is the common, healthy case).
func RemoveLegacyProjectContainers(engineBin, serviceName string) []string {
	if engineBin == "" || serviceName == "" {
		return nil
	}
	legacyProject := "citadel-" + serviceName
	out, err := exec.Command(engineBin, "ps", "-aq",
		"--filter", "label=com.docker.compose.project="+legacyProject).Output()
	if err != nil {
		return nil
	}
	ids := strings.Fields(strings.TrimSpace(string(out)))
	if len(ids) == 0 {
		return nil
	}
	args := append([]string{"rm", "-f"}, ids...)
	if err := exec.Command(engineBin, args...).Run(); err != nil {
		return nil
	}
	return ids
}
