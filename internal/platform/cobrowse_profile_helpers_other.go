//go:build !windows

package platform

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Budget for helpers that outlive the main browser process. Vars for tests.
var (
	profileReapAttempts = 50
	profileReapDelay    = 100 * time.Millisecond
)

// listProcCmdlines returns every readable process's cmdline (NUL turned into
// spaces) by PID. A package var so tests need no real /proc or browser.
var listProcCmdlines = func() map[int]string {
	out := map[int]string{}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil || len(raw) == 0 {
			continue
		}
		out[pid] = strings.ReplaceAll(string(raw), "\x00", " ")
	}
	return out
}

// reapProfileUsers kills and waits out browser helpers still using profileDir.
// Killing the main browser PID alone leaves its network, GPU and renderer
// helpers alive for a moment, and Chromium helpers recreate directories under
// --user-data-dir (MkdirAll), so a removal that races them "succeeds" and the
// tree reappears (citadel-cli#1836). A helper is identified by carrying this
// session's exact --user-data-dir argument, which is unique per session.
// Returns an error if any helper is still alive after the budget.
func reapProfileUsers(profileDir string) error {
	if profileDir == "" {
		return nil
	}
	needle := "--user-data-dir=" + profileDir
	var alive []int
	for i := 0; i < profileReapAttempts; i++ {
		alive = alive[:0]
		for pid, cmd := range listProcCmdlines() {
			if pid == os.Getpid() || !cmdHasArg(cmd, needle) {
				continue
			}
			alive = append(alive, pid)
			_ = pidKiller(pid)
		}
		if len(alive) == 0 {
			return nil
		}
		time.Sleep(profileReapDelay)
	}
	return fmt.Errorf("browser helpers %v still use %s", alive, profileDir)
}

// cmdHasArg reports whether the space-joined cmdline has arg as a whole
// argument (so session-1 never matches session-10).
func cmdHasArg(cmdline, arg string) bool {
	for _, f := range strings.Split(cmdline, " ") {
		if f == arg {
			return true
		}
	}
	return false
}
