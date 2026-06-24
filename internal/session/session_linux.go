//go:build linux

package session

import (
	"context"
	"net"
	"os"
	"strings"
	"time"
)

// detectDesktop probes for an X11/Wayland session on Linux. It reads the live
// environment, then delegates the verdict to the pure evaluateLinux helper so
// the logic is unit-testable without mutating process env.
func detectDesktop() *DesktopInfo {
	env := linuxEnv{
		display:        os.Getenv("DISPLAY"),
		waylandDisplay: os.Getenv("WAYLAND_DISPLAY"),
		xdgSessionType: os.Getenv("XDG_SESSION_TYPE"),
	}
	info := evaluateLinux(env)

	// When a DISPLAY is advertised, best-effort confirm the X server is actually
	// reachable. A stale DISPLAY pointing at a dead server should not be reported
	// as an available desktop. Wayland sockets are not probed here (they live
	// under XDG_RUNTIME_DIR and absence of the var already gates them).
	if info.HasDesktop && env.waylandDisplay == "" && env.display != "" {
		if !x11Reachable(env.display) {
			info.HasDesktop = false
			info.SessionType = "headless"
			info.Reason = "DISPLAY=" + env.display + " set but X server not reachable"
		}
	}
	return info
}

// x11Reachable parses a DISPLAY string and attempts a short TCP/unix probe to
// the X server. It returns true on a successful connect. Parsing failures or a
// missing socket return false. This is best-effort: any positive signal that
// the server accepts connections counts as reachable.
func x11Reachable(display string) bool {
	host, dpy := parseDisplay(display)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var d net.Dialer

	if host == "" {
		// Local display uses the abstract/filesystem unix socket /tmp/.X11-unix/X<n>.
		sock := "/tmp/.X11-unix/X" + dpy
		if conn, err := d.DialContext(ctx, "unix", sock); err == nil {
			conn.Close()
			return true
		}
		// Some systems only expose the abstract socket; fall back to localhost TCP.
		host = "127.0.0.1"
	}
	port := 6000 + atoiSafe(dpy)
	addr := net.JoinHostPort(host, itoa(port))
	if conn, err := d.DialContext(ctx, "tcp", addr); err == nil {
		conn.Close()
		return true
	}
	return false
}

// parseDisplay splits an X DISPLAY value "host:display.screen" into host and
// the display number. An empty host means a local (unix-socket) display.
func parseDisplay(display string) (host, dpy string) {
	idx := strings.LastIndex(display, ":")
	if idx < 0 {
		return "", "0"
	}
	host = display[:idx]
	rest := display[idx+1:]
	if dot := strings.IndexByte(rest, '.'); dot >= 0 {
		rest = rest[:dot]
	}
	if rest == "" {
		rest = "0"
	}
	return host, rest
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
