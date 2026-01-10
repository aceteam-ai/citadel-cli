// internal/platform/clipboard.go
package platform

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// CopyToClipboard copies the given text to the system clipboard.
// Returns nil on success, or an error if clipboard access failed.
func CopyToClipboard(text string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbcopy")
	case "linux":
		// Try different clipboard utilities in order of preference
		if isCommandAvailable("xclip") {
			cmd = exec.Command("xclip", "-selection", "clipboard")
		} else if isCommandAvailable("xsel") {
			cmd = exec.Command("xsel", "--clipboard", "--input")
		} else if isCommandAvailable("wl-copy") {
			// Wayland clipboard
			cmd = exec.Command("wl-copy")
		} else {
			return fmt.Errorf("no clipboard utility found (install xclip, xsel, or wl-copy)")
		}
	case "windows":
		// Use PowerShell's Set-Clipboard cmdlet
		cmd = exec.Command("powershell", "-command", "Set-Clipboard", "-Value", text)
	default:
		return fmt.Errorf("clipboard not supported on %s", runtime.GOOS)
	}

	// For Windows PowerShell, the text is passed as an argument, not via stdin
	if runtime.GOOS != "windows" {
		cmd.Stdin = strings.NewReader(text)
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to copy to clipboard: %w", err)
	}

	return nil
}
