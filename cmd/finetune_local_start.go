package cmd

import "github.com/aceteam-ai/citadel-cli/internal/jobs"

// withLocalServiceStartGuard is the shared local CLI/TUI module-start seam.
// The job handler owns the manifest-tag check and holds the fine-tune
// reservation lock through both desired-status changes and service startup.
func withLocalServiceStartGuard(configDir, name string, start func() error) error {
	return jobs.NewServiceHandler(configDir).WithHeldServiceGuard(name, start)
}
