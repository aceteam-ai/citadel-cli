//go:build !windows

package platform

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

func TestListMacReapProcsSpacedProfilePath(t *testing.T) {
	dir := "/Volumes/Data HD/Library/Caches/Citadel/cobrowse-sessions/abc"
	sibling := dir + "2"
	ps := func() (string, error) {
		return fmt.Sprintf(" 11 501 /Applications/Google Chrome.app/x --headless=new --user-data-dir=%s --no-first-run\n"+
			" 12 501 /Applications/Google Chrome.app/h --database=%s/Crash Reports --type=utility\n"+
			" 13 501 /Applications/Google Chrome.app/x --user-data-dir=%s --no-first-run\n"+
			" 14 501 /usr/bin/vim notes\n", dir, dir, sibling), nil
	}
	procs, err := listMacReapProcsFrom(ps, func(pid int) (int64, error) { return int64(pid), nil }, func(int) error { return nil })
	if err != nil || len(procs) != 3 {
		t.Fatalf("listed %v, %v", procs, err)
	}
	got := map[int]bool{}
	for _, p := range procs {
		got[p.pid] = reapUsesProfile(p, dir)
	}
	if !got[11] || !got[12] {
		t.Errorf("a spaced profile path must match its browser and crashpad helper: %v", got)
	}
	if got[13] {
		t.Errorf("a longer sibling path must not match: %v", got)
	}

	// End to end through the reaper: only the profile's users are signalled.
	prevL, prevK, prevG, prevE := listReapProcs, signalReapProc, lookupReapProc, reapEUID
	defer func() { listReapProcs, signalReapProc, lookupReapProc, reapEUID = prevL, prevK, prevG, prevE }()
	live := map[int]reapProc{}
	for _, p := range procs {
		live[p.pid] = p
	}
	var killed []int
	listReapProcs = func() ([]reapProc, error) {
		var out []reapProc
		for _, p := range live {
			out = append(out, p)
		}
		return out, nil
	}
	lookupReapProc = func(pid int) (reapProc, bool) { p, ok := live[pid]; return p, ok }
	reapEUID = func() int { return 501 }
	signalReapProc = func(pid int) error { killed = append(killed, pid); delete(live, pid); return nil }
	if err := reapProfileUsers(dir, ""); err != nil {
		t.Fatal(err)
	}
	for _, pid := range killed {
		if pid == 13 || pid == 14 {
			t.Errorf("signalled unrelated pid %d", pid)
		}
	}
	if len(killed) != 2 {
		t.Errorf("killed %v, want the two profile users", killed)
	}
}

func TestListMacReapProcsFailsClosedOnPSError(t *testing.T) {
	boom := errors.New("ps timed out")
	if _, err := listMacReapProcsFrom(func() (string, error) { return "", boom }, nil, nil); !errors.Is(err, boom) {
		t.Fatalf("want the ps error, got %v", err)
	}
	prev := listReapProcs
	defer func() { listReapProcs = prev }()
	listReapProcs = func() ([]reapProc, error) { return nil, boom }
	if err := reapProfileUsers("/p/x", ""); err == nil || !errors.Is(err, boom) {
		t.Errorf("an unreadable process list must fail the reap, got %v", err)
	}
}

func TestListMacReapProcsUnreadableStartTime(t *testing.T) {
	dir := "/p/sess"
	ps := func() (string, error) {
		return " 21 501 chrome --user-data-dir=" + dir + "\n 22 501 vim x\n", nil
	}
	noStart := func(int) (int64, error) { return 0, errors.New("sysctl") }
	// Gone since the listing: omitted, scan succeeds.
	procs, err := listMacReapProcsFrom(ps, noStart, func(int) error { return syscall.ESRCH })
	if err != nil || len(procs) != 0 {
		t.Errorf("gone: %v, %v", procs, err)
	}
	// Alive, or alive but not ours to signal: an error, never a silent drop.
	for _, serr := range []error{nil, syscall.EPERM} {
		if _, err := listMacReapProcsFrom(ps, noStart, func(int) error { return serr }); err == nil {
			t.Errorf("sig0 %v: unreadable start time of a live profile user must be an error", serr)
		}
	}
}
