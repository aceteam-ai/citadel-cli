// internal/platform/cobrowse_identity.go
//
// Identity verification for the session-sweep kill path (issue #793 security review).
//
// sweep() reads PIDs out of a pidfile written by a PRIOR process and, without this
// file, SIGKILLed them on trust alone. That is unsafe on its own: PIDs are recycled by
// the OS, the session base dir survives a reboot (plain /tmp, often not tmpfs on
// Ubuntu servers), and `citadel work` frequently runs as root under systemd. A reboot
// can hand a recorded browserPID/xvfbPID to an unrelated process (sshd, a user shell,
// ...) before the next sweep runs, and sweep would kill it.
//
// processMatchesRole closes that gap the same way reclaimStalePort (cobrowse_orphan.go)
// already closes an analogous one for the port-owner path: verify the PID is still
// what we think it is before touching it. Here "still what we think it is" means its
// /proc/<pid>/cmdline names the browser/Xvfb binary sweep expects for that role.
//
// Fails CLOSED throughout: an unreadable cmdline (process gone, EPERM, or no /proc at
// all -- this is a Linux-first node feature, see CLAUDE.md) or a marker mismatch both
// mean "do not kill", never "kill anyway". A skipped kill still lets sweep clean up
// the stale session dir/pidfile; only the kill itself is withheld.
package platform

import (
	"fmt"
	"os"
	"strings"
)

// cobrowseProcRole identifies which recorded PID in a session pidfile is being
// verified, so processMatchesRole knows which binary markers to require.
type cobrowseProcRole int

const (
	roleCobrowseBrowser cobrowseProcRole = iota
	roleCobrowseXvfb
)

// browserCmdlineMarkers mirrors findChromium's candidate binary names (cobrowse.go):
// any one appearing in the PID's cmdline is enough to call it "still the browser we
// launched". "chromium-browser" is covered by the "chromium" substring.
var browserCmdlineMarkers = []string{"google-chrome", "chromium", "chrome"}

// xvfbCmdlineMarker mirrors the literal "Xvfb" binary sweep expects for xvfbPID.
const xvfbCmdlineMarker = "xvfb"

// procCmdlineReader returns the raw /proc/<pid>/cmdline contents for a PID. A package
// var (mirroring portOwnerLookup / pidKiller in cobrowse_orphan.go) so tests can
// substitute deterministic content for a PID that may not even exist, instead of
// depending on a real /proc filesystem or a real chromium/Xvfb process. Returns an
// error when the cmdline cannot be read: process gone, permission denied, or no /proc
// (non-Linux) -- all of which the caller must treat as "unverifiable", not "verified".
var procCmdlineReader = defaultProcCmdlineReader

func defaultProcCmdlineReader(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// processMatchesRole reports whether pid's cmdline still names the binary expected for
// role. Fails closed: pid<=0, an unreadable cmdline, or no marker match all return
// false. Callers MUST NOT kill a PID this returns false for.
func processMatchesRole(pid int, role cobrowseProcRole) bool {
	if pid <= 0 {
		return false
	}
	raw, err := procCmdlineReader(pid)
	if err != nil || raw == "" {
		return false
	}
	// /proc/<pid>/cmdline is NUL-separated argv; normalize to spaces for a simple
	// case-insensitive substring match against the expected binary marker(s).
	cmdline := strings.ToLower(strings.ReplaceAll(raw, "\x00", " "))
	switch role {
	case roleCobrowseBrowser:
		for _, marker := range browserCmdlineMarkers {
			if strings.Contains(cmdline, marker) {
				return true
			}
		}
		return false
	case roleCobrowseXvfb:
		return strings.Contains(cmdline, xvfbCmdlineMarker)
	default:
		return false
	}
}
