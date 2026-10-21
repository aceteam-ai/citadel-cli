//go:build !linux && !darwin

package workflow

import (
	"context"
	"os/exec"
)

func newWorkflowShellCommand(ctx context.Context, command string) *exec.Cmd {
	// Preserve the existing /bin/sh availability requirement. This is not a
	// Windows process-tree containment implementation or native acceptance.
	return exec.CommandContext(ctx, "/bin/sh", "-c", command)
}
