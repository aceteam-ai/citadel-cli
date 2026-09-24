package cmd

import (
	"os"
	"path/filepath"

	"github.com/aceteam-ai/citadel-cli/internal/finetunesafety"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

// withLocalServiceStartGuard is the shared local CLI/TUI module-start seam.
// The job handler owns the manifest-tag check and holds the fine-tune
// reservation lock through both desired-status changes and service startup.
func withLocalServiceStartGuard(configDir, name string, start func() error) error {
	return jobs.NewServiceHandler(configDir).WithHeldServiceGuard(name, start)
}

func withIncomingServiceStartGuard(configDir, name, incomingComposePath string, incomingRequiresGPU bool, start func() error) error {
	return jobs.NewServiceHandler(configDir).WithHeldServiceGuardIncoming(name, incomingComposePath, incomingRequiresGPU, start)
}

// No-start writers still share the lock so a guarded CPU start cannot read
// yesterday's compose and then run after an installer overwrites it with GPU.
func withLocalServiceMutationLock(configDir string, mutate func() error) error {
	return finetunesafety.WithExclusive(finetunesafety.Dir(configDir), mutate)
}

// Resolve the node dir without creating a bootstrap manifest. A fresh run/add
// must acquire reservation.lock before findOrCreateManifest writes anything.
func localServiceConfigDir() (string, error) {
	if _, dir, err := findAndReadManifest(); err == nil {
		return dir, nil
	}
	if override := resolveNodeDirOverride(); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "citadel-node"), nil
}
