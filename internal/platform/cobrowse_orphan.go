// internal/platform/cobrowse_orphan.go
//
// Orphan / stale-port handling for the co-browse manager (issue #396).
//
// Two failure modes this file addresses, both observed live on node 1054:
//
// Stale :9222 owner: when a prior co-browse Chromium survives a worker restart
// (see the teardown gap below) it keeps holding the CDP debug port. A fresh
// Start()'s waitForCDP would then connect to that ORPHAN and wrongly report
// running:true for a browser this manager never launched -- so m.cmd tracks
// nothing while CDP answers from a ghost, and every later action fails
// ErrNotStarted. reclaimStalePort kills the pre-existing port owner before
// launch so m.cmd always tracks the real browser.
//
// Orphan accumulation across restarts: Stop() only kills the browser/Xvfb this
// in-memory manager launched (m.cmd / m.xvfb). When citadel work is SIGKILLed or
// crashes, that graceful path never runs and the headed Chromium plus its Xvfb
// leak; the NEXT process starts with a fresh singleton (m.cmd == nil) that cannot
// see them. reclaimStalePort gives Start() a way to reclaim the port owner
// regardless of which process launched it.
//
// The port-owner lookup and kill are split into small seams (portOwnerLookup,
// pidKiller) so the reclaim logic is unit-testable without launching Chromium or
// binding the real CDP port.
package platform

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// portOwnerLookup finds the PID listening on a loopback TCP port. It is a
// package var so tests can substitute a deterministic implementation instead of
// shelling out to lsof/ss on the test host. Returns (pid, true) when a listener
// is found, (0, false) when the port is free or the lookup tooling is absent.
var portOwnerLookup = defaultPortOwnerPID

// pidKiller terminates a process by PID. A package var for the same
// test-injection reason as portOwnerLookup. Returns nil when the process is
// gone (including "already finished").
var pidKiller = defaultKillPID

// defaultPortOwnerPID returns the PID of the process listening on 127.0.0.1:port
// using whichever of lsof / ss is available. A missing tool or no listener
// yields (0, false) rather than an error -- the caller treats "unknown owner"
// the same as "no owner" and proceeds to launch, letting waitForCDP be the
// backstop.
func defaultPortOwnerPID(port int) (int, bool) {
	if isCommandAvailable("lsof") {
		// -t: terse (PID only); -i: TCP listener on the port; -sTCP:LISTEN
		// restricts to the listening socket so we get the server, not a client.
		out, err := exec.Command(
			"lsof", "-t", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN",
		).Output()
		if err == nil {
			if pid, ok := firstPID(string(out)); ok {
				return pid, true
			}
		}
	}
	if isCommandAvailable("ss") {
		// ss prints "pid=NNNN" in the process column with -p.
		out, err := exec.Command(
			"ss", "-ltnp", fmt.Sprintf("sport = :%d", port),
		).Output()
		if err == nil {
			if pid, ok := pidFromSS(string(out)); ok {
				return pid, true
			}
		}
	}
	return 0, false
}

// firstPID parses the first PID from lsof -t output (one PID per line).
func firstPID(out string) (int, bool) {
	for _, line := range strings.Fields(out) {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && pid > 0 {
			return pid, true
		}
	}
	return 0, false
}

// pidFromSS extracts the first pid=NNNN token from `ss -p` output.
func pidFromSS(out string) (int, bool) {
	const marker = "pid="
	idx := strings.Index(out, marker)
	if idx < 0 {
		return 0, false
	}
	rest := out[idx+len(marker):]
	end := strings.IndexAny(rest, ", \t\n")
	if end >= 0 {
		rest = rest[:end]
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil && pid > 0 {
		return pid, true
	}
	return 0, false
}

// defaultKillPID sends SIGKILL to a PID and waits, bounded, for it to disappear.
// "process already finished" / "no such process" are treated as success -- the
// goal state is "PID is gone", which is already true. Returns an error only when
// the process is still alive after the grace window.
func defaultKillPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil // no handle -> treat as gone
	}
	if err := proc.Kill(); err != nil {
		if isProcessGoneErr(err) {
			return nil
		}
		return fmt.Errorf("kill pid %d: %w", pid, err)
	}
	// Wait for the OS to actually reap/kill it so a follow-up port bind does not
	// race a still-dying process holding the socket.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(pid) {
		return fmt.Errorf("pid %d still alive after SIGKILL", pid)
	}
	return nil
}

// pidAlive reports whether a PID is still a live process. signal 0 probes
// existence without delivering a signal; ESRCH means gone.
func pidAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// EPERM means it exists but we may not own it; ESRCH means gone.
	return !strings.Contains(err.Error(), "no such process") &&
		!strings.Contains(err.Error(), "process already finished")
}

// isProcessGoneErr reports whether an error from Process.Kill/Signal means the
// process is already gone (so callers can treat teardown as successful).
func isProcessGoneErr(err error) bool {
	if err == nil {
		return true
	}
	if err == os.ErrProcessDone {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "process already finished") ||
		strings.Contains(msg, "no such process")
}

// reclaimStalePort kills whatever process currently listens on the CDP debug
// port, if any. Called by Start() when the manager is NOT tracking a live
// browser (m.cmd is nil/dead) yet the port is occupied -- an orphan from a prior
// crashed/killed worker. Returns (reclaimed, err): reclaimed is true when a stale
// owner was found and killed. A missing lookup tool yields (false, nil): the
// caller proceeds and lets the browser launch / waitForCDP surface any conflict.
func reclaimStalePort(port int) (bool, error) {
	pid, ok := portOwnerLookup(port)
	if !ok || pid <= 0 {
		return false, nil
	}
	// Never kill our own process (should not happen, but be defensive).
	if pid == os.Getpid() {
		return false, nil
	}
	if err := pidKiller(pid); err != nil {
		return false, fmt.Errorf("reclaim stale CDP port %d (pid %d): %w", port, pid, err)
	}
	return true, nil
}
