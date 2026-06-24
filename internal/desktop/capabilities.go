package desktop

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/session"
)

const detectionTimeout = 5 * time.Second

type Capabilities struct {
	OS               string `json:"os"`
	OSVersion        string `json:"os_version"`
	Display          string `json:"display"`
	ScreenResolution string `json:"screen_resolution,omitempty"`
	VNCPort          int    `json:"vnc_port,omitempty"`

	// Session reports whether this process has an interactive desktop session
	// and which desktop-dependent sub-capabilities (vnc/screenshot/input/
	// terminal) are usable. Populated by the per-OS session probe. The server
	// and frontend gate desktop affordances on this so a headless node does not
	// advertise (or get asked to perform) VNC/screenshot/input actions.
	Session *session.DesktopInfo `json:"session,omitempty"`
}

func DetectCapabilities() *Capabilities {
	caps := &Capabilities{
		OS:      runtime.GOOS,
		Session: session.DetectDesktop(),
	}

	switch runtime.GOOS {
	case "linux":
		caps.OSVersion = detectLinuxVersion()
		caps.Display = detectLinuxDisplay()
		if caps.Display != "" {
			caps.ScreenResolution = detectLinuxResolution(caps.Display)
		}
		caps.VNCPort = detectVNCPort()
	case "darwin":
		// TODO: sw_vers for version, system_profiler SPDisplaysDataType for resolution
		// screencapture for screenshots, cliclick for input actions
		caps.OSVersion = "unknown"
		caps.Display = displayFromSession(caps.Session)
	case "windows":
		// TODO: PowerShell Get-CimInstance Win32_OperatingSystem for version
		// Get-CimInstance Win32_VideoController for display
		caps.OSVersion = "unknown"
		caps.Display = displayFromSession(caps.Session)
	}

	return caps
}

// displayFromSession derives the legacy Display string from the session probe
// on platforms where we have no richer display detection yet. Returns
// "available" only when an interactive desktop is present, otherwise "".
// This makes the previously-unconditional "available" honest on headless nodes.
func displayFromSession(s *session.DesktopInfo) string {
	if s != nil && s.HasDesktop {
		return "available"
	}
	return ""
}

func detectLinuxVersion() string {
	return ParseOSRelease(readFileContents("/etc/os-release"))
}

// ParseOSRelease extracts PRETTY_NAME from os-release file contents.
func ParseOSRelease(contents string) string {
	if contents == "" {
		return "unknown"
	}
	scanner := bufio.NewScanner(strings.NewReader(contents))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			value := strings.TrimPrefix(line, "PRETTY_NAME=")
			value = strings.Trim(value, "\"'")
			if value != "" {
				return value
			}
		}
	}
	return "unknown"
}

func detectLinuxDisplay() string {
	if display := os.Getenv("DISPLAY"); display != "" {
		return display
	}
	if wayland := os.Getenv("WAYLAND_DISPLAY"); wayland != "" {
		return "wayland:" + wayland
	}
	return ""
}

func detectLinuxResolution(display string) string {
	ctx, cancel := context.WithTimeout(context.Background(), detectionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "xrandr", "--query")
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return ParseXrandrOutput(string(output))
}

var xrandrConnectedRe = regexp.MustCompile(`^\S+\s+connected\s+(?:primary\s+)?(\d+x\d+)`)
var xrandrResolutionRe = regexp.MustCompile(`^\s+(\d+x\d+)\s+.*\*`)

// ParseXrandrOutput extracts the current resolution from xrandr output.
func ParseXrandrOutput(output string) string {
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		if m := xrandrConnectedRe.FindStringSubmatch(scanner.Text()); len(m) > 1 {
			return m[1]
		}
	}
	scanner = bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		if m := xrandrResolutionRe.FindStringSubmatch(scanner.Text()); len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

func detectVNCPort() int {
	ctx, cancel := context.WithTimeout(context.Background(), detectionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ss", "-tlnp")
	output, err := cmd.Output()
	if err != nil {
		return 0
	}
	outputStr := string(output)
	for _, port := range []int{5900, 5901} {
		if strings.Contains(outputStr, fmt.Sprintf(":%d ", port)) {
			return port
		}
	}
	return 0
}

func readFileContents(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
