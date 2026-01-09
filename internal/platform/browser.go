// internal/platform/browser.go
package platform

import (
	"fmt"
	"os/exec"
	"runtime"
)

// OpenURL opens a URL in the default browser
// Works on Linux and macOS
func OpenURL(url string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to open browser: %w", err)
	}

	return nil
}
