// cmd/tailscale.go
// Tailscale network helper functions
package cmd

import (
	"os"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// getTailscaleCLI returns the path to the tailscale CLI executable.
// On Windows, we need to use the full path because the PATH might not be updated
// in child processes (especially when launched via cmd /c from init).
func getTailscaleCLI() string {
	if platform.IsWindows() {
		// Standard installation path for Tailscale on Windows
		fullPath := `C:\Program Files\Tailscale\tailscale.exe`
		if _, err := os.Stat(fullPath); err == nil {
			return fullPath
		}
		// Fall back to PATH if the standard location doesn't exist
	}
	return "tailscale"
}
