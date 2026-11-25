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
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aceteam-ai/citadel-cli/internal/finetunesafety"
)

// The worker and the Control Center can mutate the same legacy service in
// parallel within one process. Keep cleanup out of the backup window.
var legacyCleanupMu sync.Mutex

// The persistent advisory lock also covers separate Citadel processes running
// as the same user. Never remove its file: replacing the inode would split the
// lock between old and new holders.
func withLegacyMutationLock(engineBin, serviceName string, fn func() error) error {
	root, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("locate compose mutation lock: %w", err)
	}
	return withLegacyMutationLockAt(root, engineBin, serviceName, fn)
}

func withLegacyMutationLockAt(root, engineBin, serviceName string, fn func() error) error {
	key := sha256.Sum256([]byte(engineBin + "\x00" + serviceName))
	return finetunesafety.WithExclusive(filepath.Join(root, "citadel", "compose-mutation", fmt.Sprintf("%x", key)), fn)
}

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
	var removed []string
	if err := withLegacyMutationLock(engineBin, serviceName, func() error {
		removed = removeLegacyProjectContainersLocked(engineBin, serviceName)
		return nil
	}); err != nil {
		// Cleanup is best effort. Refusing on lock contention keeps a backup
		// available for the replacement that owns the lock.
		return nil
	}
	return removed
}

func removeLegacyProjectContainersLocked(engineBin, serviceName string) []string {
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
