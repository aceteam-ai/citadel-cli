//go:build !windows

package platform

import (
	"testing"
	"time"
)

// Helpers carrying the session's exact --user-data-dir are killed and waited
// out; other sessions' helpers (including a longer sibling path) are left alone.
func TestReapProfileUsersKillsOnlyThisSessionsHelpers(t *testing.T) {
	const dir = "/s/cb-1"
	procs := map[int]string{
		101: "chrome --type=utility --user-data-dir=" + dir + " --foo",
		102: "chrome --type=gpu-process --user-data-dir=" + dir,
		103: "chrome --user-data-dir=" + dir + "0",
		104: "chrome --user-data-dir=/s/cb-2",
	}
	prevL, prevK := listProcCmdlines, pidKiller
	t.Cleanup(func() { listProcCmdlines, pidKiller = prevL, prevK })
	var killed []int
	listProcCmdlines = func() map[int]string {
		c := map[int]string{}
		for k, v := range procs {
			c[k] = v
		}
		return c
	}
	pidKiller = func(pid int) error {
		killed = append(killed, pid)
		delete(procs, pid)
		return nil
	}
	if err := reapProfileUsers(dir); err != nil {
		t.Fatal(err)
	}
	if len(killed) != 2 {
		t.Fatalf("killed %v, want exactly 101 and 102", killed)
	}
	if _, ok := procs[103]; !ok {
		t.Fatal("a sibling session with a longer path was killed")
	}
	if _, ok := procs[104]; !ok {
		t.Fatal("another session's helper was killed")
	}
}

// A helper that survives the budget is reported, never silently ignored.
func TestReapProfileUsersReportsSurvivor(t *testing.T) {
	prevL, prevK, prevA, prevD := listProcCmdlines, pidKiller, profileReapAttempts, profileReapDelay
	t.Cleanup(func() {
		listProcCmdlines, pidKiller, profileReapAttempts, profileReapDelay = prevL, prevK, prevA, prevD
	})
	listProcCmdlines = func() map[int]string { return map[int]string{7: "chrome --user-data-dir=/s/x"} }
	pidKiller = func(int) error { return nil }
	profileReapAttempts, profileReapDelay = 3, time.Millisecond
	if err := reapProfileUsers("/s/x"); err == nil {
		t.Fatal("expected an error for a helper that never exits")
	}
}
