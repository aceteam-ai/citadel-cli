// cmd/tailscale.go
// Tailscale network helper functions
package cmd

import (
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// getTailscaleCLI returns the path to the tailscale CLI executable.
// Delegates to the centralized platform.GetTailscaleCLI() which handles
// PATH lookup and platform-specific fallback locations for Windows, macOS, and Linux.
func getTailscaleCLI() string {
	return platform.GetTailscaleCLI()
}
