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
// RemoveLegacyProjectContainers removes legacy per-service containers so both
// up and down converge under the default project. A replacement backup is
// excluded while its new container is being verified.
package compose

import (
	"os/exec"
	"strings"
	"sync"
)

// The worker and the Control Center can mutate the same legacy service in
// parallel within one process. Keep cleanup out of the backup window.
var legacyCleanupMu sync.Mutex

// RemoveLegacyProjectContainers force-removes all containers created under the
// legacy per-service compose project "citadel-<serviceName>". engineBin is the
// container engine binary ("docker" or "podman"). Best-effort by design: it
// returns the removed container IDs for logging, and swallows engine errors (a
// missing engine or no matching containers is the common, healthy case).
func RemoveLegacyProjectContainers(engineBin, serviceName string) []string {
	if engineBin == "" || serviceName == "" {
		return nil
	}
	legacyCleanupMu.Lock()
	defer legacyCleanupMu.Unlock()
	legacyProject := "citadel-" + serviceName
	out, err := exec.Command(engineBin, "ps", "-aq",
		"--filter", "label=com.docker.compose.project="+legacyProject,
		"--format", "{{.ID}} {{.Names}}").Output()
	if err != nil {
		return nil
	}
	ids := legacyRemovalIDs(out, BackupContainerName("citadel-"+serviceName))
	if len(ids) == 0 {
		return nil
	}
	args := append([]string{"rm", "-f"}, ids...)
	if err := exec.Command(engineBin, args...).Run(); err != nil {
		return nil
	}
	return ids
}

func legacyRemovalIDs(out []byte, backupName string) []string {
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] == backupName {
			continue
		}
		ids = append(ids, fields[0])
	}
	return ids
}
