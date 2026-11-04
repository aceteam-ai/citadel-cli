package composerefresh

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ReplaceManagedCompose publishes an already-validated template under the
// caller's service mutation lock. Recheck the bytes before replacement, then
// repair an existing ownership stamp without changing its version or peers.
// A stamp error is returned even after the compose rename; retrying the selected
// template repairs that metadata. Atomic rename does not promise crash durability.
func ReplaceManagedCompose(path, service, expected, selected string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if string(data) != expected {
		return false, fmt.Errorf("compose for %q changed during reconciliation", service)
	}
	changed := expected != selected
	if changed {
		if err := writeCompose(path, selected); err != nil {
			return false, err
		}
	}
	dir := filepath.Dir(path)
	stamp, exists := readStamp(dir)
	if exists && stamp.Hashes[service] != hashContent(selected) {
		stamp.Hashes[service] = hashContent(selected)
		encoded, err := json.MarshalIndent(stamp, "", "  ")
		if err != nil {
			return changed, err
		}
		if err := writeCompose(filepath.Join(dir, StampFileName), string(encoded)); err != nil {
			return changed, fmt.Errorf("update managed compose stamp: %w", err)
		}
	}
	return changed, nil
}
