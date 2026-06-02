package desktop

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"
)

const screenshotTimeout = 10 * time.Second

// CaptureScreenshot captures a PNG screenshot of the current display.
func CaptureScreenshot(ctx context.Context) ([]byte, error) {
	switch runtime.GOOS {
	case "linux":
		return captureLinux(ctx)
	case "darwin":
		// TODO: screencapture -x -t png /dev/stdout
		return nil, fmt.Errorf("screenshot not implemented on macOS")
	case "windows":
		// TODO: PowerShell Add-Type + System.Drawing to capture screen
		return nil, fmt.Errorf("screenshot not implemented on Windows")
	default:
		return nil, fmt.Errorf("screenshot not supported on %s", runtime.GOOS)
	}
}

func captureLinux(ctx context.Context) ([]byte, error) {
	display := os.Getenv("DISPLAY")
	if display == "" {
		display = ":0"
	}

	captureCtx, cancel := context.WithTimeout(ctx, screenshotTimeout)
	defer cancel()

	env := append(os.Environ(), "DISPLAY="+display)

	if path, err := exec.LookPath("import"); err == nil {
		cmd := exec.CommandContext(captureCtx, path, "-window", "root", "png:-")
		cmd.Env = env
		output, err := cmd.Output()
		if err == nil && len(output) > 0 {
			return output, nil
		}
	}

	if path, err := exec.LookPath("scrot"); err == nil {
		cmd := exec.CommandContext(captureCtx, path, "-o", "-")
		cmd.Env = env
		output, err := cmd.Output()
		if err == nil && len(output) > 0 {
			return output, nil
		}
	}

	return nil, fmt.Errorf("no screenshot tool available (install imagemagick or scrot)")
}
