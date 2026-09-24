package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/aceteam-ai/citadel-cli/internal/finetunesafety"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// withLocalServiceStartGuard is the shared local CLI/TUI module-start seam.
// The job handler owns the manifest-tag check and holds the fine-tune
// reservation lock through both desired-status changes and service startup.
func withLocalServiceStartGuard(configDir, name string, start func() error) error {
	return withLocalServiceStartGuardAfterLock(configDir, name, nil, func(nodeDirSource) error { return start() })
}

func withLocalServiceStartGuardSource(configDir, name string, start func(nodeDirSource) error) error {
	return withLocalServiceStartGuardAfterLock(configDir, name, nil, start)
}

// afterLock is only a deterministic test seam. The canonical identity is
// verified after jobs has acquired reservation.lock and before user mutation.

func withLocalServiceStartGuardAfterLock(configDir, name string, afterLock func() error, start func(nodeDirSource) error) error {
	return withCanonicalNodePointerLock(configDir, func(source nodeDirSource) error {
		return jobs.NewServiceHandler(configDir).WithHeldServiceGuard(name, func() error {
			if afterLock != nil {
				if err := afterLock(); err != nil {
					return err
				}
			}
			if _, err := assertCanonicalNodeDir(configDir); err != nil {
				return err
			}
			return start(source)
		})
	})
}

func withIncomingServiceStartGuard(configDir, name, incomingComposePath string, incomingRequiresGPU bool, start func() error) error {
	return withIncomingServiceStartGuardSource(configDir, name, incomingComposePath, incomingRequiresGPU, func(nodeDirSource) error { return start() })
}

func withIncomingServiceStartGuardSource(configDir, name, incomingComposePath string, incomingRequiresGPU bool, start func(nodeDirSource) error) error {
	return withCanonicalNodePointerLock(configDir, func(source nodeDirSource) error {
		return jobs.NewServiceHandler(configDir).WithHeldServiceGuardIncoming(name, incomingComposePath, incomingRequiresGPU, func() error {
			if _, err := assertCanonicalNodeDir(configDir); err != nil {
				return err
			}
			return start(source)
		})
	})
}

// No-start writers still share the lock so a guarded CPU start cannot read
// yesterday's compose and then run after an installer overwrites it with GPU.
func withLocalServiceMutationLock(configDir string, mutate func() error) error {
	return withLocalServiceMutationLockSource(configDir, func(nodeDirSource) error { return mutate() })
}

func withLocalServiceMutationLockSource(configDir string, mutate func(nodeDirSource) error) error {
	return withCanonicalNodePointerLock(configDir, func(source nodeDirSource) error {
		return finetunesafety.WithExclusive(finetunesafety.Dir(configDir), func() error {
			if _, err := assertCanonicalNodeDir(configDir); err != nil {
				return err
			}
			return mutate(source)
		})
	})
}

// Lock order is always global pointer then per-node reservation. A writer that
// retargets config.yaml must acquire the pointer lock; a local start holds it
// through the final compose-up so a held trainer on another node cannot become
// the canonical node mid-start.
func withCanonicalNodePointerLock(configDir string, fn func(nodeDirSource) error) error {
	return withNodePointerLock(filepath.Join(platform.ConfigDir(), "config.yaml"), func() error {
		source, err := assertCanonicalNodeDir(configDir)
		if err != nil {
			return err
		}
		return fn(source)
	})
}

func withNodePointerLock(globalConfigFile string, fn func() error) error {
	return finetunesafety.WithExclusive(globalConfigFile+".pointer-safety", fn)
}

func assertCanonicalNodeDir(configDir string) (nodeDirSource, error) {
	current, source, err := resolveNodeConfigDirReadOnly()
	if err != nil {
		return source, fmt.Errorf("verify node configuration: %w", err)
	}
	if filepath.Clean(configDir) != current {
		return source, fmt.Errorf("node configuration changed during service admission: locked %s, current %s", configDir, current)
	}
	return source, nil
}

// Resolve the node dir without creating a bootstrap manifest. A fresh run/add
// must acquire reservation.lock before findOrCreateManifest writes anything.
func localServiceConfigDir() (string, error) {
	dir, _, err := resolveNodeConfigDirReadOnly()
	return dir, err
}
