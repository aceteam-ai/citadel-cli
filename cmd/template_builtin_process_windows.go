//go:build windows

package cmd

import (
	"fmt"
)

// This is an intentionally unreachable compile stub. Rendering fails closed in
// newRenderLimitedCommand on every non-Linux platform before process-tree
// configuration. Keep the stub only because the shared builtin code must still
// cross-compile; it is not a partial Windows isolation fallback.
func configurePapercraftProcessTree(*renderCommand) func() error {
	return func() error {
		return fmt.Errorf("render process isolation is unsupported on Windows")
	}
}
