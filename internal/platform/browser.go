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
		// Try multiple browser openers in order of preference
		if isCommandAvailable("xdg-open") {
			cmd = exec.Command("xdg-open", url)
		} else if isCommandAvailable("sensible-browser") {
			cmd = exec.Command("sensible-browser", url)
		} else if isCommandAvailable("firefox") {
			cmd = exec.Command("firefox", url)
		} else if isCommandAvailable("google-chrome") {
			cmd = exec.Command("google-chrome", url)
		} else if isCommandAvailable("chromium-browser") {
			cmd = exec.Command("chromium-browser", url)
		} else {
			return fmt.Errorf("no browser found (install xdg-open or a browser)")
		}
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to open browser: %w", err)
	}

	return nil
}
