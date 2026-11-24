//go:build !windows

package platform

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHelpers stands in for /proc: a table of helper processes, a signal that
// removes them (optionally running a side effect, like a dying writer), and the
// identity lookups the reaper re-checks before it signals.
type fakeHelpers struct {
	mu      sync.Mutex
	procs   map[int]reapProc
	onKill  func(pid int)
	killed  []int
	listed  int
	scopeOK func(pid int, unit string) bool
	euid    int
}

func installFakeHelpers(t *testing.T, f *fakeHelpers) {
	t.Helper()
	pl, pk, pg, pe, ps := listReapProcs, signalReapProc, lookupReapProc, reapEUID, reapInScope
	pa, pd, pb := profileReapDelay, profileReapBudget, sessionRemoveDelay
	t.Cleanup(func() {
		listReapProcs, signalReapProc, lookupReapProc, reapEUID, reapInScope = pl, pk, pg, pe, ps
		profileReapDelay, profileReapBudget, sessionRemoveDelay = pa, pd, pb
	})
	profileReapDelay, profileReapBudget, sessionRemoveDelay = time.Millisecond, 100*time.Millisecond, time.Millisecond
	reapEUID = func() int { return f.euid }
	listReapProcs = func() ([]reapProc, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.listed++
		var out []reapProc
		for _, p := range f.procs {
			out = append(out, p)
		}
		return out, nil
	}
	lookupReapProc = func(pid int) (reapProc, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p, ok := f.procs[pid]
		return p, ok
	}
	signalReapProc = func(pid int) error {
		f.mu.Lock()
		f.killed = append(f.killed, pid)
		delete(f.procs, pid)
		cb := f.onKill
		f.mu.Unlock()
		if cb != nil {
			cb(pid)
		}
		return nil
	}
	reapInScope = func(pid int, unit string) bool {
		if f.scopeOK == nil {
			return true
		}
		return f.scopeOK(pid, unit)
	}
}

func helperProc(pid int, start uint64, uid int, args ...string) reapProc {
	return reapProc{pid: pid, start: start, uid: uid, args: args}
}

func TestReapMatchesOnlyThisSessionsHelpers(t *testing.T) {
	const dir = "/s/cb-1 with space"
	f := &fakeHelpers{euid: 1000, procs: map[int]reapProc{
		1: helperProc(1, 5, 1000, "chrome", "--type=utility", "--user-data-dir="+dir),
		2: helperProc(2, 5, 1000, "chrome_crashpad_handler", "--database="+dir+"/Crash Reports"),
		3: helperProc(3, 5, 1000, "chrome", "--user-data-dir="+dir+"0"),
		4: helperProc(4, 5, 1000, "chrome", "--user-data-dir=/s/cb-2"),
		5: helperProc(5, 5, 1000, "sh", "-c", "chrome --user-data-dir="+dir+" --x"),
		6: helperProc(6, 5, 1000, "strace", "-p", "1", dir),
	}}
	installFakeHelpers(t, f)
	if err := reapProfileUsers(dir, ""); err != nil {
		t.Fatal(err)
	}
	if len(f.killed) != 2 {
		t.Fatalf("killed %v, want exactly 1 and 2", f.killed)
	}
	for _, pid := range []int{3, 4, 5, 6} {
		if _, ok := f.procs[pid]; !ok {
			t.Fatalf("pid %d must not be killed", pid)
		}
	}
}

func TestReapIdentityGuards(t *testing.T) {
	const dir = "/s/cb-1"
	arg := "--user-data-dir=" + dir
	f := &fakeHelpers{euid: 1000, procs: map[int]reapProc{
		1: helperProc(1, 5, 0, "chrome", arg),    // another user's process
		2: helperProc(2, 5, 1000, "chrome", arg), // not in the session's scope
		3: helperProc(3, 5, 1000, "chrome", arg), // recycled between list and kill
	}}
	installFakeHelpers(t, f)
	f.scopeOK = func(pid int, unit string) bool { return unit == "app-x.scope" && pid != 2 }
	lookupReapProc = func(pid int) (reapProc, bool) {
		p, ok := f.procs[pid]
		if pid == 3 {
			p.start = 99 // same pid, new process
		}
		return p, ok
	}
	profileReapBudget = 20 * time.Millisecond
	_ = reapProfileUsers(dir, "app-x.scope")
	if len(f.killed) != 0 {
		t.Fatalf("signalled %v; uid, scope and start-time guards must each refuse", f.killed)
	}
}

// profile that records the order Persist runs in.
type orderedProfile struct {
	*fakeProfile
	onPersist func(dir string)
}

func (p *orderedProfile) Persist(dir string) error {
	p.onPersist(dir)
	return p.fakeProfile.Persist(dir)
}

func startOrdered(t *testing.T, f *fakeHelpers, onPersist func(string)) (*CobrowseSessionManager, CobrowseSessionStatus, *orderedProfile) {
	t.Helper()
	l := &sentinelCheckingLauncher{}
	l.install(t)
	installFakeHelpers(t, f)
	m := newCobrowseSessionManager(trustedBaseDir(t), 2)
	p := &orderedProfile{fakeProfile: &fakeProfile{}, onPersist: onPersist}
	st, err := m.StartSessionWithProfile("", p)
	if err != nil {
		t.Fatal(err)
	}
	return m, st, p
}

// Helpers are reaped, then the profile is sealed, then the dir is removed.
func TestTeardownReapsBeforePersistThenRemoves(t *testing.T) {
	f := &fakeHelpers{euid: os.Geteuid(), procs: map[int]reapProc{}}
	var events []string
	var m *CobrowseSessionManager
	var st CobrowseSessionStatus
	m, st, p := startOrdered(t, f, func(dir string) {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("Persist ran after the dir was removed: %v", err)
		}
		events = append(events, "persist")
	})
	f.procs[10] = helperProc(10, 7, os.Geteuid(), "chrome", "--user-data-dir="+st.Profile)
	f.onKill = func(int) { events = append(events, "kill") }
	if err := m.Stop(st.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if strings.Join(events, ",") != "kill,persist" {
		t.Fatalf("order = %v, want kill then persist", events)
	}
	if _, err := os.Lstat(st.Profile); !os.IsNotExist(err) {
		t.Fatalf("dir not removed: %v", err)
	}
	if p.closed != 1 {
		t.Fatalf("closed = %d", p.closed)
	}
}

// A helper that will not die: nothing is sealed over the previous ciphertext,
// the dir is still removed, and Stop reports it.
func TestTeardownSurvivorFailsClosed(t *testing.T) {
	f := &fakeHelpers{euid: os.Geteuid(), procs: map[int]reapProc{}}
	m, st, p := startOrdered(t, f, func(string) {})
	f.procs[10] = helperProc(10, 7, os.Geteuid(), "chrome", "--user-data-dir="+st.Profile)
	signalReapProc = func(int) error { return nil } // survives SIGKILL for the test
	err := m.Stop(st.ID)
	if err == nil || !strings.Contains(err.Error(), "still use") {
		t.Fatalf("Stop = %v, want the survivor error", err)
	}
	if p.persisted != 0 {
		t.Fatalf("persisted %d times with a live helper; the old ciphertext must be kept", p.persisted)
	}
	if p.closed != 1 {
		t.Fatalf("closed = %d, the key must still be released", p.closed)
	}
	if _, serr := os.Lstat(st.Profile); !os.IsNotExist(serr) {
		t.Fatalf("decrypted dir left behind: %v", serr)
	}
}

// A helper that appears only after the first removal and recreates the tree
// when killed (the #1836 shape) is caught by the post-removal re-reap.
func TestTeardownCatchesRecreateAfterRemove(t *testing.T) {
	f := &fakeHelpers{euid: os.Geteuid(), procs: map[int]reapProc{}}
	m, st, _ := startOrdered(t, f, func(string) {})
	listed := 0
	listReapProcs = func() ([]reapProc, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		listed++
		if listed == 1 {
			return nil, nil // main process just died, no helper visible yet
		}
		var out []reapProc
		for _, p := range f.procs {
			out = append(out, p)
		}
		return out, nil
	}
	f.procs[10] = helperProc(10, 7, os.Geteuid(), "chrome", "--user-data-dir="+st.Profile)
	f.onKill = func(int) { // dying helper recreates files
		_ = os.MkdirAll(filepath.Join(st.Profile, "Default"), 0o700)
		_ = os.WriteFile(filepath.Join(st.Profile, "Default", "late"), []byte("x"), 0o600)
	}
	if err := m.Stop(st.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Lstat(st.Profile); !os.IsNotExist(err) {
		t.Fatalf("recreated dir survived Stop: %v", err)
	}
	if len(f.killed) != 1 {
		t.Fatalf("killed = %v", f.killed)
	}
}

// An unreadable /proc must not read as "no processes": a reap, a seal and a
// failed recovery launch would otherwise treat a surviving browser as gone.
func TestListProcForReapFailsClosedWithoutProc(t *testing.T) {
	prevRoot, prevList := reapProcRoot, listReapProcs
	t.Cleanup(func() { reapProcRoot, listReapProcs = prevRoot, prevList })
	reapProcRoot = filepath.Join(t.TempDir(), "missing-proc")
	if procs, err := listProcForReap(); err == nil {
		t.Fatalf("listProcForReap with no /proc = %v, nil; want an error", procs)
	}
	listReapProcs = listProcForReap
	if err := reapProfileUsers(t.TempDir(), ""); err == nil {
		t.Fatal("reapProfileUsers must fail closed when the process list cannot be read")
	}
}
