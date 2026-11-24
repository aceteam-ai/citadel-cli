package platform

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func macProbe(files map[string]bool, env map[string]string, path map[string]string) macBrowserProbe {
	return macBrowserProbe{
		getenv: func(k string) string { return env[k] },
		home:   "/Users/u",
		isExec: func(p string) bool { return files[p] },
		lookPath: func(n string) (string, error) {
			if p, ok := path[n]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
	}
}

const (
	macChrome = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	macEdge   = "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"
	macUserCh = "/Users/u/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	macCanary = "/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary"
)

func TestDiscoverMacBrowserOrder(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		env   map[string]string
		path  map[string]string
		want  string
		kind  string
	}{
		{"chrome first", []string{macEdge, macChrome}, nil, nil, macChrome, "chrome"},
		{"user apps chrome", []string{macEdge, macUserCh}, nil, nil, macUserCh, "chrome"},
		{"edge when no chrome", []string{macEdge}, nil, nil, macEdge, "edge"},
		{"canary last of the apps", []string{macCanary}, nil, nil, macCanary, "chrome"},
		{"path fallback", nil, nil, map[string]string{"chromium": "/opt/homebrew/bin/chromium"}, "/opt/homebrew/bin/chromium", "chromium"},
		{"env path wins", []string{macChrome, "/x/Browser"}, map[string]string{EnvCobrowseBrowser: "/x/Browser"}, nil, "/x/Browser", "chromium"},
		{"env name edge", []string{macChrome, macEdge}, map[string]string{EnvCobrowseBrowser: "Edge"}, nil, macEdge, "edge"},
		{"env bundle", []string{macEdge}, map[string]string{EnvCobrowseBrowser: "/Applications/Microsoft Edge.app/"}, nil, macEdge, "edge"},
	}
	for _, c := range cases {
		files := map[string]bool{}
		for _, f := range c.files {
			files[f] = true
		}
		info, err := discoverMacBrowser(macProbe(files, c.env, c.path))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if info.Path != c.want || info.Kind != c.kind {
			t.Errorf("%s: got %+v, want %s (%s)", c.name, info, c.want, c.kind)
		}
	}
}

func TestDiscoverMacBrowserErrors(t *testing.T) {
	_, err := discoverMacBrowser(macProbe(nil, nil, nil))
	if !errors.Is(err, ErrNoBrowser) {
		t.Fatalf("want ErrNoBrowser, got %v", err)
	}
	for _, want := range []string{macChrome, macUserCh, macEdge, macCanary, "PATH:chromium", EnvCobrowseBrowser} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
	// A pinned name never falls back to another browser.
	files := map[string]bool{macChrome: true}
	_, err = discoverMacBrowser(macProbe(files, map[string]string{EnvCobrowseBrowser: "edge"}, nil))
	if !errors.Is(err, ErrNoBrowser) || !strings.Contains(err.Error(), "found no edge") {
		t.Errorf("pinned edge should fail, got %v", err)
	}
	_, err = discoverMacBrowser(macProbe(files, map[string]string{EnvCobrowseBrowser: "firefox"}, nil))
	if !errors.Is(err, ErrNoBrowser) {
		t.Errorf("unknown name should fail, got %v", err)
	}
	_, err = discoverMacBrowser(macProbe(files, map[string]string{EnvCobrowseBrowser: "/Applications/Other.app"}, nil))
	if !errors.Is(err, ErrNoBrowser) {
		t.Errorf("unknown bundle should fail, got %v", err)
	}
}

func TestMacBrowserKind(t *testing.T) {
	for p, want := range map[string]string{
		macChrome: "chrome", macEdge: "edge", "/a/Chromium": "chromium", macCanary: "chrome", "/a/whatever": "chromium",
	} {
		if got := macBrowserKind(p); got != want {
			t.Errorf("%s: %s, want %s", p, got, want)
		}
	}
}

func TestMacDefaultBaseDir(t *testing.T) {
	got, err := macDefaultBaseDir("/Users/u", nil)
	if err != nil || got != filepath.Join("/Users/u/Library/Caches", "Citadel", cobrowseSessionsDirName) {
		t.Errorf("home dir: %s, %v", got, err)
	}
	for _, home := range []string{"", "relative"} {
		if got, err := macDefaultBaseDir(home, nil); err == nil || got != "" {
			t.Errorf("home %q must be refused, got %q, %v", home, got, err)
		}
	}
	if _, err := macDefaultBaseDir("/Users/u", errors.New("no home")); err == nil {
		t.Error("a home lookup error must be refused, never a shared temp fallback")
	}
}

func TestGroupGuardOrderAndNoSignalAfterTeardown(t *testing.T) {
	var events []string
	exited := make(chan struct{})
	g := &groupGuard{
		pgid:          42,
		browserExited: exited,
		termGrace:     time.Second,
		signal: func(pg int, sig syscall.Signal) error {
			events = append(events, fmt.Sprintf("%d:%v", pg, sig))
			if sig == syscall.SIGTERM {
				close(exited) // the browser exits on TERM
			}
			return nil
		},
		reapLeader: func() { events = append(events, "reap") },
	}
	if err := g.teardown(); err != nil {
		t.Fatal(err)
	}
	want := []string{"42:terminated", "42:killed", "reap"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Errorf("order %v, want %v", events, want)
	}
	// Stop after the waiter's teardown (or the reverse): no further signal.
	if err := g.teardown(); err != nil || len(events) != 3 {
		t.Errorf("second teardown: %v, events %v", err, events)
	}
}

func TestGroupGuardSerializesAndPropagatesErrors(t *testing.T) {
	exited := make(chan struct{})
	var mu sync.Mutex
	inSignal, overlap, calls := 0, false, 0
	g := &groupGuard{
		pgid:          7,
		browserExited: exited,
		termGrace:     10 * time.Millisecond,
		signal: func(int, syscall.Signal) error {
			mu.Lock()
			inSignal++
			calls++
			if inSignal > 1 {
				overlap = true
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			inSignal--
			mu.Unlock()
			return syscall.EPERM
		},
		reapLeader: func() {},
	}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = g.teardown() }(i)
	}
	wg.Wait()
	if overlap || calls != 2 {
		t.Errorf("concurrent teardown must signal exactly TERM+KILL once: overlap %v calls %d", overlap, calls)
	}
	for _, e := range errs {
		if e == nil || !errors.Is(e, syscall.EPERM) {
			t.Errorf("signal failure must be propagated to every caller, got %v", e)
		}
	}
}

func TestParseMacPS(t *testing.T) {
	out := "  101   501 /Applications/Google Chrome.app/x --user-data-dir=/a b --headless=new\n 7 0 /sbin/launchd\nbogus\n  x 1 y\n"
	rows := parseMacPS(out)
	if len(rows) != 2 || rows[0].pid != 101 || rows[0].uid != 501 ||
		rows[0].cmd != "/Applications/Google Chrome.app/x --user-data-dir=/a b --headless=new" || rows[1].cmd != "/sbin/launchd" {
		t.Errorf("rows: %+v", rows)
	}
}

func TestDiscoverMacBrowserNamedOverrideFromPath(t *testing.T) {
	info, err := discoverMacBrowser(macProbe(nil, map[string]string{EnvCobrowseBrowser: "edge"}, map[string]string{"microsoft-edge": "/opt/bin/microsoft-edge"}))
	if err != nil || info.Path != "/opt/bin/microsoft-edge" || info.Kind != "edge" {
		t.Errorf("got %+v, %v", info, err)
	}
	// A named override never takes another browser from PATH.
	if _, err := discoverMacBrowser(macProbe(nil, map[string]string{EnvCobrowseBrowser: "edge"}, map[string]string{"chromium": "/opt/bin/chromium"})); !errors.Is(err, ErrNoBrowser) {
		t.Errorf("want ErrNoBrowser, got %v", err)
	}
}

func TestMacOwnerAliveWithStart(t *testing.T) {
	alive := func(int) error { return nil }
	gone := func(int) error { return syscall.ESRCH }
	denied := func(int) error { return syscall.EPERM }
	start := func(v int64) func(int) (int64, error) { return func(int) (int64, error) { return v, nil } }
	failing := func(int) (int64, error) { return 0, errors.New("sysctl") }
	cases := []struct {
		name  string
		pid   int
		st    int64
		sig   func(int) error
		start func(int) (int64, error)
		want  bool
	}{
		{"no pid", 0, 5, alive, start(5), false},
		{"gone", 10, 5, gone, start(5), false},
		{"same start", 10, 5, alive, start(5), true},
		{"reused pid", 10, 5, alive, start(6), false},
		{"unrecorded start errs alive", 10, 0, alive, start(6), true},
		{"unreadable start errs alive", 10, 5, alive, failing, true},
		{"permission denied still checks start", 10, 5, denied, start(6), false},
		{"permission denied same start", 10, 5, denied, start(5), true},
	}
	for _, c := range cases {
		if got := macOwnerAliveWithStart(c.pid, c.st, c.sig, c.start); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestMacCmdlineHasProfile(t *testing.T) {
	dir := "/Users/u/Library/Caches/Citadel/cobrowse-sessions/abc123"
	for cmd, want := range map[string]bool{
		"/Applications/X --headless=new --user-data-dir=" + dir + " --no-first-run": true,
		"/Applications/X --user-data-dir=" + dir:                                    true,
		"/Applications/X --user-data-dir=" + dir + "4 --no-first-run":               false,
		"/Applications/X --user-data-dir=/other":                                    false,
		"/bin/zsh -c echo " + dir:                                                   false,
	} {
		if got := macCmdlineHasProfile(cmd, dir); got != want {
			t.Errorf("%q: %v, want %v", cmd, got, want)
		}
	}
	if macCmdlineHasProfile("--user-data-dir=", "") || macCmdlineHasProfile("--user-data-dir=rel", "rel") {
		t.Error("empty or relative dir must never match")
	}
}

type fakeMacProcs struct {
	cmdline  map[int]string
	pgid     map[int]int
	killed   []int
	removed  []string
	owners   map[int]int64 // alive owners: pid -> start
	killFail bool
	unread   map[int]bool     // pids whose command line cannot be read
	reapErrs map[string]error // profile-user scan result by session dir name
	reaped   []string
}

func (f *fakeMacProcs) deps(self int, selfStart int64) macSweepDeps {
	return macSweepDeps{
		self:      self,
		selfStart: selfStart,
		ownPgrp:   5,
		ownerAlive: func(pid int, start int64) bool {
			s, ok := f.owners[pid]
			return ok && (start == 0 || s == start)
		},
		cmdline: func(pid int) (string, error) {
			if f.unread[pid] {
				return "", errors.New("ps failed")
			}
			if c, ok := f.cmdline[pid]; ok {
				return c, nil
			}
			return "", errMacProcGone
		},
		pgid: func(pid int) (int, error) {
			if g, ok := f.pgid[pid]; ok {
				return g, nil
			}
			return 0, errors.New("gone")
		},
		killGroup: func(g int) error {
			f.killed = append(f.killed, g)
			if f.killFail {
				return syscall.EPERM
			}
			for pid, pg := range f.pgid {
				if pg == g {
					delete(f.cmdline, pid)
				}
			}
			return nil
		},
		removeDir: func(d string) error { f.removed = append(f.removed, d); return os.RemoveAll(d) },
		reapUsers: func(d string) error {
			f.reaped = append(f.reaped, d)
			return f.reapErrs[filepath.Base(d)]
		},
	}
}

func macSessionDir(t *testing.T, base, id, pidfile string) string {
	t.Helper()
	dir := filepath.Join(base, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if pidfile != "" {
		if err := os.WriteFile(filepath.Join(dir, sessionPidfileName), []byte(pidfile), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSweepMacSessions(t *testing.T) {
	base := filepath.Join(t.TempDir(), "sessions")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	const self, selfStart = 100, int64(1000)
	live := macSessionDir(t, base, "live", fmt.Sprintf("%d\n201\n0\n%d\n", 300, 3000))     // owner alive, same start
	reused := macSessionDir(t, base, "reused", fmt.Sprintf("%d\n202\n0\n%d\n", 300, 2999)) // owner pid reused
	mine := macSessionDir(t, base, "mine", fmt.Sprintf("%d\n203\n0\n%d\n", self, selfStart))
	pred := macSessionDir(t, base, "pred", fmt.Sprintf("%d\n204\n0\n%d\n", self, selfStart-1)) // dead predecessor whose pid we reused
	dead := macSessionDir(t, base, "dead", "400\n205\n0\n4000\n")
	deadReused := macSessionDir(t, base, "deadreused", "401\n206\n0\n4000\n") // browser pid now unrelated
	fresh := macSessionDir(t, base, "fresh", "")
	stale := macSessionDir(t, base, "stale", "")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	f := &fakeMacProcs{
		owners:  map[int]int64{300: 3000},
		cmdline: map[int]string{205: "/Applications/Google Chrome.app/x --user-data-dir=" + dead + " --headless=new", 206: "/usr/sbin/sshd"},
		pgid:    map[int]int{205: 205, 206: 206},
	}
	sweepMacSessions(base, f.deps(self, selfStart))
	for _, keep := range []string{live, mine, fresh} {
		if _, err := os.Lstat(keep); err != nil {
			t.Errorf("%s must be kept: %v", keep, err)
		}
	}
	for _, gone := range []string{reused, pred, dead, deadReused, stale} {
		if _, err := os.Lstat(gone); err == nil {
			t.Errorf("%s must be removed", gone)
		}
	}
	if len(f.killed) != 1 || f.killed[0] != 205 {
		t.Errorf("only the verified browser group may be killed, got %v", f.killed)
	}
}

func TestKillVerifiedBrowserGroupFailsClosed(t *testing.T) {
	dir := "/tmp/sess"
	f := &fakeMacProcs{
		cmdline: map[int]string{
			10: "chrome --user-data-dir=" + dir, 11: "chrome --user-data-dir=" + dir, 12: "chrome --user-data-dir=/other",
			14: "chrome --user-data-dir=" + dir, 15: "chrome --user-data-dir=" + dir,
		},
		pgid:   map[int]int{10: 9, 11: 7, 12: 12, 14: 5, 15: 15},
		unread: map[int]bool{16: true},
	}
	d := f.deps(1, 0)
	// Verified member of the recorded group: killed and confirmed gone.
	if err := killVerifiedBrowserGroup(dir, 10, 9, d); err != nil || len(f.killed) != 1 || f.killed[0] != 9 {
		t.Errorf("verified member: %v, killed %v", err, f.killed)
	}
	// Wrong group, our own group, and a pre-placeholder record whose pid does
	// not lead its group are refused with an error and nothing is signalled.
	for _, c := range []struct{ pid, leader int }{{11, 9}, {14, 5}, {15, 0 + 3}} {
		if err := killVerifiedBrowserGroup(dir, c.pid, c.leader, d); err == nil {
			t.Errorf("pid %d leader %d must be refused", c.pid, c.leader)
		}
	}
	// Unreadable command line: cannot verify, so an error (dir is kept).
	if err := killVerifiedBrowserGroup(dir, 16, 0, d); err == nil {
		t.Error("an unverifiable browser must be an error")
	}
	// Gone, other program, and pid 0 or 1: nothing of ours left, no error, no signal.
	for _, pid := range []int{13, 12, 0, 1} {
		if err := killVerifiedBrowserGroup(dir, pid, 0, d); err != nil {
			t.Errorf("pid %d: %v", pid, err)
		}
	}
	if len(f.killed) != 1 {
		t.Errorf("killed %v", f.killed)
	}
	// A failing signal is propagated.
	f2 := &fakeMacProcs{cmdline: map[int]string{20: "chrome --user-data-dir=" + dir}, pgid: map[int]int{20: 19}, killFail: true}
	if err := killVerifiedBrowserGroup(dir, 20, 19, f2.deps(1, 0)); err == nil || !errors.Is(err, syscall.EPERM) {
		t.Errorf("kill failure must propagate, got %v", err)
	}
}

func TestSweepKeepsDirWhenBrowserCannotBeKilledOrVerified(t *testing.T) {
	base := filepath.Join(t.TempDir(), "sessions")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	stuck := macSessionDir(t, base, "stuck", "400\n205\n9\n4000\n")
	unread := macSessionDir(t, base, "unread", "401\n206\n0\n4000\n")
	f := &fakeMacProcs{
		cmdline:  map[int]string{205: "chrome --user-data-dir=" + stuck},
		pgid:     map[int]int{205: 9},
		unread:   map[int]bool{206: true},
		killFail: true,
	}
	sweepMacSessions(base, f.deps(1, 0))
	for _, dir := range []string{stuck, unread} {
		if _, err := os.Stat(filepath.Join(dir, sessionPidfileName)); err != nil {
			t.Errorf("%s and its pid record must be kept for a retry: %v", dir, err)
		}
	}
	if len(f.removed) != 0 {
		t.Errorf("removed %v", f.removed)
	}
}

func TestSweepMacSessionsRefusesUntrustedBase(t *testing.T) {
	base := filepath.Join(t.TempDir(), "sessions")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o777); err != nil {
		t.Fatal(err)
	}
	dir := macSessionDir(t, base, "x", "400\n205\n0\n")
	f := &fakeMacProcs{cmdline: map[int]string{205: "chrome --user-data-dir=" + dir}, pgid: map[int]int{205: 205}}
	sweepMacSessions(base, f.deps(1, 0))
	if len(f.killed) != 0 || len(f.removed) != 0 {
		t.Errorf("a group/other-writable base must be left alone: killed %v removed %v", f.killed, f.removed)
	}
}

func TestBuildChromeArgsMacHeadless(t *testing.T) {
	args := buildChromeArgs(cobrowseLaunchOptions{
		profileDir: "/p", headless: true, windowSize: "1280,720", mockKeychain: true,
		proxyServer: "http://127.0.0.1:9", quiet: true, stealth: true,
	})
	joined := " " + strings.Join(args, " ") + " "
	for _, want := range []string{
		" --headless=new ", " --allow-browser-signin=false ", " --disable-sync ", " --use-mock-keychain ",
		" --proxy-server=http://127.0.0.1:9 ", " --proxy-bypass-list=<-loopback> ", " --disable-extensions ", " --disable-quic ",
		" --disable-background-networking ",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %v", want, args)
		}
	}
	plain := " " + strings.Join(buildChromeArgs(cobrowseLaunchOptions{profileDir: "/p"}), " ") + " "
	if strings.Contains(plain, "--use-mock-keychain") {
		t.Error("mock keychain must be opt-in")
	}
}

func TestSweepScansProfileUsersBeforeRemoving(t *testing.T) {
	base := filepath.Join(t.TempDir(), "sessions")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	// Browser pids are gone in all three; only the helper scan differs.
	helper := macSessionDir(t, base, "helper", "400\n301\n9\n4000\n")
	scanfail := macSessionDir(t, base, "scanfail", "401\n302\n9\n4000\n")
	clean := macSessionDir(t, base, "clean", "402\n303\n9\n4000\n")
	f := &fakeMacProcs{reapErrs: map[string]error{
		"helper":   errors.New("browser helpers [77] still use the profile"),
		"scanfail": errors.New("cannot list processes: ps timed out"),
	}}
	sweepMacSessions(base, f.deps(1, 0))
	for _, dir := range []string{helper, scanfail} {
		if _, err := os.Stat(filepath.Join(dir, sessionPidfileName)); err != nil {
			t.Errorf("%s and its record must be kept: %v", dir, err)
		}
	}
	if _, err := os.Lstat(clean); err == nil {
		t.Error("a clean scan must let the dir be removed")
	}
	if len(f.reaped) != 3 {
		t.Errorf("every orphan dir must be scanned, got %v", f.reaped)
	}
}
