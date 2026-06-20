package desktop

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// VNCReadinessError describes why a VNC readiness check failed, providing
// actionable diagnostics rather than a bare "not ready" message.
type VNCReadinessError struct {
	// Reason is a machine-parsable category.
	Reason string
	// Detail is a human-readable explanation of what went wrong and how to fix it.
	Detail string
}

func (e *VNCReadinessError) Error() string {
	return fmt.Sprintf("VNC not ready: %s (%s)", e.Reason, e.Detail)
}

// VNC readiness failure reasons.
const (
	ReasonNoDisplay     = "no_display"
	ReasonPortNotOpen   = "port_not_open"
	ReasonDialFailed    = "dial_failed"
	ReasonNoVNCServer   = "no_vnc_server"
	ReasonNoScreenTools = "no_screen_tools"
	ReasonPlatformUnsup = "platform_unsupported"
)

// CheckVNCReady probes whether the VNC server at the given address is accepting
// TCP connections. It returns nil on success or a *VNCReadinessError with
// diagnostic detail on failure.
func CheckVNCReady(address string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return &VNCReadinessError{
			Reason: ReasonDialFailed,
			Detail: fmt.Sprintf("cannot connect to VNC server at %s: %v", address, err),
		}
	}
	conn.Close()
	return nil
}

// WaitForVNCReady polls the VNC server address with exponential backoff until
// it accepts a TCP connection or the context expires. Returns nil on success.
//
// The initial interval is 200ms, doubling each attempt up to maxInterval.
// This is a pure net.DialTimeout loop with no external dependencies, making
// it safe to use on any platform.
func WaitForVNCReady(ctx context.Context, address string, maxWait time.Duration) error {
	deadline, cancel := context.WithTimeout(ctx, maxWait)
	defer cancel()

	interval := 200 * time.Millisecond
	maxInterval := 2 * time.Second

	for {
		select {
		case <-deadline.Done():
			return &VNCReadinessError{
				Reason: ReasonPortNotOpen,
				Detail: fmt.Sprintf("VNC server at %s did not become ready within %s", address, maxWait),
			}
		default:
		}

		conn, err := net.DialTimeout("tcp", address, 1*time.Second)
		if err == nil {
			conn.Close()
			return nil
		}

		select {
		case <-deadline.Done():
			return &VNCReadinessError{
				Reason: ReasonPortNotOpen,
				Detail: fmt.Sprintf("VNC server at %s did not become ready within %s (last error: %v)", address, maxWait, err),
			}
		case <-time.After(interval):
		}

		if interval < maxInterval {
			interval *= 2
			if interval > maxInterval {
				interval = maxInterval
			}
		}
	}
}

// DiagnoseDesktopReadiness checks whether the desktop environment is usable
// for screenshot capture and input actions. Returns nil if ready, or a
// *VNCReadinessError with an actionable diagnostic if not.
func DiagnoseDesktopReadiness() error {
	switch runtime.GOOS {
	case "linux":
		return diagnoseLinuxDesktop()
	case "windows":
		// Windows GDI capture is always available when a desktop session exists.
		return nil
	case "darwin":
		return &VNCReadinessError{
			Reason: ReasonPlatformUnsup,
			Detail: "desktop capture is not yet implemented on macOS",
		}
	default:
		return &VNCReadinessError{
			Reason: ReasonPlatformUnsup,
			Detail: fmt.Sprintf("desktop capture is not supported on %s", runtime.GOOS),
		}
	}
}

func diagnoseLinuxDesktop() error {
	// Check for a display server
	display := os.Getenv("DISPLAY")
	wayland := os.Getenv("WAYLAND_DISPLAY")
	if display == "" && wayland == "" {
		return &VNCReadinessError{
			Reason: ReasonNoDisplay,
			Detail: "no DISPLAY or WAYLAND_DISPLAY environment variable set; " +
				"ensure an X11 or Wayland session is running, or start a virtual framebuffer (Xvfb)",
		}
	}

	// Check for screenshot tools
	hasImport := false
	hasScrot := false
	if _, err := exec.LookPath("import"); err == nil {
		hasImport = true
	}
	if _, err := exec.LookPath("scrot"); err == nil {
		hasScrot = true
	}
	if !hasImport && !hasScrot {
		return &VNCReadinessError{
			Reason: ReasonNoScreenTools,
			Detail: "no screenshot tool available; install imagemagick (for 'import') or scrot: " +
				"apt-get install imagemagick or apt-get install scrot",
		}
	}

	return nil
}

// DiagnoseVNCServer checks whether a VNC server process is running and
// listening on the expected port. Returns nil if a server is detected.
func DiagnoseVNCServer(expectedPort int) error {
	switch runtime.GOOS {
	case "linux":
		return diagnoseLinuxVNCServer(expectedPort)
	case "windows":
		return diagnoseWindowsVNCServer()
	default:
		return &VNCReadinessError{
			Reason: ReasonPlatformUnsup,
			Detail: fmt.Sprintf("VNC server management not supported on %s", runtime.GOOS),
		}
	}
}

func diagnoseLinuxVNCServer(expectedPort int) error {
	// Check if x11vnc process is running
	if err := exec.Command("pgrep", "-x", "x11vnc").Run(); err != nil {
		return &VNCReadinessError{
			Reason: ReasonNoVNCServer,
			Detail: "x11vnc is not running; start it with: citadel vnc enable, " +
				"or install it with: apt-get install x11vnc",
		}
	}

	// Check if the expected port is listening
	if err := CheckVNCReady(fmt.Sprintf("127.0.0.1:%d", expectedPort), 2*time.Second); err != nil {
		return &VNCReadinessError{
			Reason: ReasonPortNotOpen,
			Detail: fmt.Sprintf("x11vnc is running but port %d is not accepting connections; "+
				"check if x11vnc is bound to a different port or if another process is using port %d",
				expectedPort, expectedPort),
		}
	}

	return nil
}

func diagnoseWindowsVNCServer() error {
	cmd := exec.Command("sc", "query", "tvnserver")
	output, err := cmd.Output()
	if err != nil {
		return &VNCReadinessError{
			Reason: ReasonNoVNCServer,
			Detail: "TightVNC service not found; install TightVNC or enable it with: citadel vnc enable",
		}
	}
	if !strings.Contains(string(output), "RUNNING") {
		return &VNCReadinessError{
			Reason: ReasonNoVNCServer,
			Detail: "TightVNC service is installed but not running; start it with: citadel vnc enable",
		}
	}
	return nil
}
