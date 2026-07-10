package desktop

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
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
	display, xauthority := platform.ResolveX11Env()

	captureCtx, cancel := context.WithTimeout(ctx, screenshotTimeout)
	defer cancel()

	env := append(os.Environ(), "DISPLAY="+display)
	if xauthority != "" {
		env = append(env, "XAUTHORITY="+xauthority)
	}

	// Capture candidates in priority order. We try each that resolves and, if a
	// tool is present but FAILS (a non-X11 ImageMagick build, or a display it
	// cannot open), we fall through and keep the real error -- so the final
	// message is actionable instead of the old misleading "no tool available".
	//
	// /usr/bin/import is tried before a bare "import" because a source-built
	// /usr/local/bin/import (no X11 delegate) commonly shadows the apt one on
	// PATH and would otherwise be picked and silently fail.
	candidates := []struct {
		name string
		args []string
	}{
		{"scrot", []string{"-o", "-"}},
		{"/usr/bin/import", []string{"-window", "root", "png:-"}},
		{"import", []string{"-window", "root", "png:-"}},
	}

	var lastErr error
	triedAny := false
	for _, c := range candidates {
		path, err := exec.LookPath(c.name)
		if err != nil {
			continue
		}
		triedAny = true
		cmd := exec.CommandContext(captureCtx, path, c.args...)
		cmd.Env = env
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		output, runErr := cmd.Output()
		if runErr == nil && len(output) > 0 {
			return output, nil
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			if runErr != nil {
				detail = runErr.Error()
			} else {
				detail = "produced no output"
			}
		}
		lastErr = fmt.Errorf("%s: %s", c.name, detail)
	}

	if !triedAny {
		return nil, fmt.Errorf("no screenshot tool found on PATH (install scrot or imagemagick with X11 support)")
	}
	return nil, fmt.Errorf("screenshot capture failed on display %s (XAUTHORITY=%q): %w", display, xauthority, lastErr)
}
