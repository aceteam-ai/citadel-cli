package compose

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/composerefresh"
	"github.com/aceteam-ai/citadel-cli/services"
)

// ReconcileCPUComposeOnDisk changes only the GPU flavor of a known embedded
// template. Operator edits and same-flavor historical templates are preserved;
// ordinary template upgrades remain the versioned refresh sweep's job. The
// caller owns the usual service mutation lock. This does not restart a service.
func ReconcileCPUComposeOnDisk(path, name, selected, override string) (bool, error) {
	if !services.HasCPUComposeVariant(name) {
		return false, nil
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("refusing non-regular embedded compose for %q", name)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	existing := string(data)
	canonical := existing
	if strings.TrimSpace(override) != "" {
		// Only this override's exact name can be normalized for the known-hash
		// check. A foreign namespace never acquires ownership by normalization.
		canonical = strings.Replace(canonical, "container_name: "+ContainerName(name, override)+"\n", "container_name: "+ContainerName(name, "")+"\n", 1)
	}
	if !services.KnownComposeHashes[name][fmt.Sprintf("%x", sha256.Sum256([]byte(canonical)))] {
		return false, nil
	}
	if canonical != selected && services.ComposeDeclaresNvidiaReservation(canonical) == services.ComposeDeclaresNvidiaReservation(selected) {
		return false, nil
	}
	target, _, err := EnsureNamespacedContainerName(selected, name, override)
	if err != nil {
		return false, err
	}
	return composerefresh.ReplaceManagedCompose(path, name, existing, target)
}
