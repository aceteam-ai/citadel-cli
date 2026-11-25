//go:build !windows

package platform

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Budget for helpers that outlive the main browser process. Vars for tests.
var (
	profileReapBudget = 5 * time.Second // total wall clock, however many helpers
	profileReapDelay  = 100 * time.Millisecond
)

// reapProc is one process as the reaper saw it: identity (pid + start time +
// owner) and argv.
type reapProc struct {
	pid   int
	start uint64
	uid   int
	args  []string
	// cmd is the raw command line when the source has no argv boundaries (macOS
	// ps output). It is matched as a string so a profile path containing a
	// space still matches; empty when args are exact (Linux /proc).
	cmd string
}

// Seams so tests need no real /proc, browser or signals.
var (
	reapProcRoot   = "/proc"
	listReapProcs  = listProcForReap
	lookupReapProc = readProcForReap
	reapEUID       = os.Geteuid
	reapInScope    = inScope
	// signalReapProc is a non-blocking SIGKILL: it never waits for the target,
	// so one stuck process cannot eat the reap budget.
	signalReapProc = func(pid int) error {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		return nil
	}
)

// readProcForReap reads one process from /proc. On a host without /proc
// (macOS) it finds nothing, so the reap is a no-op there and helper cleanup
// relies on the main-process kill alone.
func readProcForReap(pid int) (reapProc, bool) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || len(raw) == 0 {
		return reapProc{}, false
	}
	fi, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	if err != nil {
		return reapProc{}, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return reapProc{}, false
	}
	start, alive, err := procIdentity(pid)
	if err != nil || !alive || start == 0 {
		return reapProc{}, false
	}
	return reapProc{pid: pid, start: start, uid: int(st.Uid), args: splitCmdline(raw)}, true
}

// splitCmdline splits a /proc cmdline on NUL. Only a single-field cmdline
// (a process that retitled itself) falls back to splitting on spaces.
func splitCmdline(raw []byte) []string {
	fields := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	if len(fields) == 1 && strings.Contains(fields[0], " ") {
		return strings.Split(fields[0], " ")
	}
	return fields
}

func listProcForReap() ([]reapProc, error) {
	ents, err := os.ReadDir(reapProcRoot)
	if err != nil {
		// An unreadable /proc is unknown, not "no processes": callers must not
		// seal a profile or settle a recovery launch on an empty list.
		return nil, fmt.Errorf("list processes: %w", err)
	}
	var out []reapProc
	self, sawSelf := os.Getpid(), false
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if pid == self {
			sawSelf = true
		}
		if p, ok := readProcForReap(pid); ok {
			out = append(out, p)
		}
	}
	// A real procfs always lists the scanning process. An empty or foreign
	// directory at the mount point (procfs unmounted) is unknown, not "none".
	if !sawSelf {
		return nil, fmt.Errorf("list processes: %s does not list this process; procfs may not be mounted", reapProcRoot)
	}
	return out, nil
}

// usesProfile reports whether some "--flag=value" argument names profileDir or
// a path under it (--user-data-dir, and crashpad's --database=<dir>/Crash Reports).
func usesProfile(args []string, profileDir string) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			continue
		}
		i := strings.Index(a, "=")
		if i < 0 {
			continue
		}
		v := a[i+1:]
		if v == profileDir || strings.HasPrefix(v, profileDir+"/") {
			return true
		}
	}
	return false
}

// reapUsesProfile reports whether a process names profileDir: by raw command
// line where the source has one, else by argv.
func reapUsesProfile(p reapProc, profileDir string) bool {
	if p.cmd != "" {
		return macCmdlineUsesProfile(p.cmd, profileDir)
	}
	return usesProfile(p.args, profileDir)
}

// reapProfileUsers kills and waits out browser helpers still using profileDir
// (citadel-cli#1836). Killing the main browser PID alone leaves its network,
// GPU and renderer helpers alive for a moment, and Chromium helpers recreate
// directories under the profile, so a removal that races them "succeeds" and
// the tree reappears.
//
// Kept to the argv match rather than Setpgid plus a group kill: a scoped
// launch is wrapped in systemd-run (the group is not the browser's), an
// adopted session after an exec restart has no group to signal, and Chromium
// helpers may leave the group anyway. The match is guarded instead: the
// process must be owned by this uid, in the session's scope when it has one,
// and its start time and argv are re-read immediately before a non-blocking
// SIGKILL. The whole wait is bounded by one wall-clock deadline. Returns an
// error naming any helper still alive at the deadline.
func reapProfileUsers(profileDir, scope string) error {
	if profileDir == "" {
		return nil
	}
	deadline := time.Now().Add(profileReapBudget)
	for {
		var alive []int
		procs, lerr := listReapProcs()
		if lerr != nil {
			// Fail closed: a process list that could not be read proves nothing.
			return fmt.Errorf("cannot list processes to check for browser helpers using %s: %w", profileDir, lerr)
		}
		for _, c := range procs {
			if c.pid == os.Getpid() || c.uid != reapEUID() || !reapUsesProfile(c, profileDir) {
				continue
			}
			if scope != "" && !reapInScope(c.pid, scope) {
				continue
			}
			alive = append(alive, c.pid)
			if cur, ok := lookupReapProc(c.pid); ok && cur.start == c.start && cur.uid == c.uid && reapUsesProfile(cur, profileDir) {
				_ = signalReapProc(c.pid)
			}
		}
		if len(alive) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("browser helpers %v still use %s", alive, profileDir)
		}
		time.Sleep(profileReapDelay)
	}
}
