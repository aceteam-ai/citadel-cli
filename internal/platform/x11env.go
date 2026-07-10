package platform

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// ResolveX11Env resolves the X display and Xauthority file to use for
// capturing/controlling the local desktop.
//
// This exists because Citadel commonly runs as a systemd --user service, whose
// environment does NOT inherit the graphical session's DISPLAY/XAUTHORITY. The
// old code defaulted DISPLAY to ":0" and set no XAUTHORITY, so on a normal
// GDM/Wayland box (where the session is ":1" and the server is cookie-guarded)
// every capture failed to open the display -- and then reported the misleading
// "no screenshot tool available" (aceteam-ai/citadel-cli#287 family).
//
// Precedence: an explicitly-set env var always wins. Otherwise we detect the
// active local X display and its matching Xauthority from the running X server.
func ResolveX11Env() (display, xauthority string) {
	display = strings.TrimSpace(os.Getenv("DISPLAY"))
	xauthority = strings.TrimSpace(os.Getenv("XAUTHORITY"))

	if display == "" {
		display = detectActiveDisplay()
	}
	if xauthority == "" {
		xauthority = findXAuthority(display)
	}
	return display, xauthority
}

// xorgAuthRe extracts the `-auth <path>` argument from an X server command line.
var xorgAuthRe = regexp.MustCompile(`-auth\s+(\S+)`)

// detectActiveDisplay finds the local X display number when DISPLAY is unset.
// It prefers a display backed by a live X server socket. Returns ":0" as a
// last resort so behavior never regresses below the old hardcoded default.
func detectActiveDisplay() string {
	// The X server binds a unix socket at /tmp/.X11-unix/X<N> for display :N.
	entries, err := os.ReadDir("/tmp/.X11-unix")
	if err == nil {
		var displays []string
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, "X") && len(name) > 1 {
				num := name[1:]
				if _, convErr := parseNonNegInt(num); convErr == nil {
					displays = append(displays, ":"+num)
				}
			}
		}
		// Prefer the display whose X server we can actually locate a running
		// process for (more likely the real, authenticated session).
		for _, d := range displays {
			if xorgAuthPathForDisplay(d) != "" {
				return d
			}
		}
		if len(displays) > 0 {
			return displays[0]
		}
	}
	return ":0"
}

// findXAuthority resolves the Xauthority file for a display, trying (in order):
// the running X server's own -auth path, the GDM per-user cookie, and the
// user's ~/.Xauthority. Returns "" if none exist (caller then omits XAUTHORITY).
func findXAuthority(display string) string {
	if p := xorgAuthPathForDisplay(display); p != "" {
		return p
	}
	uid := os.Getuid()
	candidates := []string{
		filepath.Join("/run/user", itoa(uid), "gdm", "Xauthority"),
		filepath.Join("/run/user", itoa(uid), ".mutter-Xwaylandauth"),
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".Xauthority"))
	}
	for _, c := range candidates {
		if fileExists(c) {
			return c
		}
	}
	return ""
}

// xorgAuthPathForDisplay scans running X server processes for one serving the
// given display and returns its `-auth` cookie path. It matches by the display
// socket file descriptor being open, falling back to the first X server found
// when the display can't be tied to a specific process (common: Xorg is
// launched with -displayfd rather than an explicit :N arg).
func xorgAuthPathForDisplay(display string) string {
	// pgrep the common X server binaries; read each cmdline for -auth.
	for _, bin := range []string{"Xorg", "X", "Xwayland"} {
		out, err := exec.Command("pgrep", "-x", bin).Output()
		if err != nil {
			continue
		}
		for _, pid := range strings.Fields(string(out)) {
			data, rErr := os.ReadFile("/proc/" + pid + "/cmdline")
			if rErr != nil {
				continue
			}
			cmdline := strings.ReplaceAll(string(data), "\x00", " ")
			if m := xorgAuthRe.FindStringSubmatch(cmdline); len(m) == 2 && fileExists(m[1]) {
				return m[1]
			}
		}
	}
	return ""
}

func parseNonNegInt(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, os.ErrInvalid
		}
		n = n*10 + int(r-'0')
	}
	if s == "" {
		return 0, os.ErrInvalid
	}
	return n, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
