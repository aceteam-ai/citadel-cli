package compose

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/composerefresh"
	"github.com/aceteam-ai/citadel-cli/services"
)

// RefreshKnownComposeOnDisk installs selected over an existing embedded compose
// file only when that file is byte-identical to a template a citadel release
// shipped (services.KnownComposeHashes), after normalizing this override's
// namespaced container name. Absent, non-regular, operator-edited and
// foreign-namespace files are left alone. It reports whether the file changed;
// a second call with the same selection is a no-op.
//
// This carries a template change to a start that runs before the worker's
// versioned sweep (composerefresh.Sweep). Callers that also reconcile the GPU
// flavor must skip this when services.SelectComposeForHost reports
// PreserveExisting. It does not restart anything: callers decide whether a
// running container must be recreated (see StartRefresh).
func RefreshKnownComposeOnDisk(path, name, selected, override string) (bool, error) {
	existing, ok := readRegularCompose(path)
	if !ok {
		return false, nil
	}
	canonical := canonicalForOverride(existing, name, override)
	if canonical == selected {
		return false, nil
	}
	if !isKnownTemplate(name, canonical) {
		return false, nil
	}
	target, _, err := EnsureNamespacedContainerName(selected, name, override)
	if err != nil {
		return false, err
	}
	return composerefresh.ReplaceManagedCompose(path, name, existing, target)
}

// IsKnownCompose reports whether the regular file at path is byte-identical
// (after normalizing this override's container name) to a template a citadel
// release shipped for name, current or historical. Such a file is citadel's,
// not an operator edit, so recreating its container to apply it is safe.
func IsKnownCompose(path, name, override string) bool {
	existing, ok := readRegularCompose(path)
	return ok && isKnownTemplate(name, canonicalForOverride(existing, name, override))
}

// canonicalForOverride undoes only this override's own container-name
// namespacing; a foreign namespace never acquires ownership by normalization.
func canonicalForOverride(content, name, override string) string {
	if strings.TrimSpace(override) == "" {
		return content
	}
	return strings.Replace(content, "container_name: "+ContainerName(name, override)+"\n", "container_name: "+ContainerName(name, "")+"\n", 1)
}

func isKnownTemplate(name, content string) bool {
	return services.KnownComposeHashes[name][fmt.Sprintf("%x", sha256.Sum256([]byte(content)))]
}

// StartRefresh records whether a start-time reconciliation rewrote a managed
// compose file, and the bytes on either side, so a failed recreate can restore
// the previous file and leave the next start to retry.
type StartRefresh struct {
	Changed  bool
	Previous string
	Current  string
}

// CaptureStartRefresh runs reconcile and reports whether it changed the
// regular file at path. A file that was absent before (first materialization)
// is not a change: no container can be running from it.
func CaptureStartRefresh(path string, reconcile func() error) (StartRefresh, error) {
	before, existed := readRegularCompose(path)
	if err := reconcile(); err != nil {
		return StartRefresh{}, err
	}
	after, exists := readRegularCompose(path)
	if !existed || !exists || before == after {
		return StartRefresh{}, nil
	}
	return StartRefresh{Changed: true, Previous: before, Current: after}, nil
}

// Rollback restores the previous compose content when nothing else replaced
// the refreshed file in the meantime. It is a no-op for an unchanged refresh.
func (r StartRefresh) Rollback(path, name string) error {
	if !r.Changed {
		return nil
	}
	_, err := composerefresh.ReplaceManagedCompose(path, name, r.Current, r.Previous)
	return err
}

func readRegularCompose(path string) (string, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(data), true
}
