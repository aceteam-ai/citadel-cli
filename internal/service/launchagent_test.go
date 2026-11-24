package service

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeLaunchctl is a launchctl runner over an in-memory job table. It records
// every call so tests can assert the order launchd was driven in.
type fakeLaunchctl struct {
	calls []string

	loaded   bool
	pid      int
	disabled bool

	bootoutFails   bool  // bootout and unload both fail and the job stays loaded
	bootoutLies    bool  // bootout reports failure but the job is gone
	bootstrapFails bool  // bootstrap fails
	bootstrapNoPID bool  // bootstrap loads the job but the worker exits at once
	hardErr        error // returned for every call: launchctl itself is unusable
	printExit      int   // non-zero: print exits with this status (113 means absent)
}

func (f *fakeLaunchctl) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if f.hardErr != nil {
		return nil, f.hardErr
	}
	exit := &LaunchctlExit{Code: 113, Output: "Could not find service"}
	switch args[0] {
	case "print":
		if f.printExit != 0 {
			return nil, &LaunchctlExit{Code: f.printExit, Output: "Could not find domain"}
		}
		if !f.loaded {
			return nil, exit
		}
		out := "gui/501/ai.aceteam.citadel = {\n\tstate = waiting\n"
		if f.pid > 0 {
			out += "\tpid = " + itoa(f.pid) + "\n"
		}
		return []byte(out + "}\n"), nil
	case "print-disabled":
		v := "enabled"
		if f.disabled {
			v = "disabled"
		}
		return []byte("disabled services = {\n\t\"com.example.other\" => enabled\n\t\"ai.aceteam.citadel\" => " + v + "\n}\n"), nil
	case "bootout":
		if f.bootoutLies {
			f.loaded, f.pid = false, 0
			return nil, exit
		}
		if f.bootoutFails {
			return nil, exit
		}
		f.loaded, f.pid = false, 0
		return nil, nil
	case "bootstrap":
		if f.bootstrapFails {
			return nil, exit
		}
		f.loaded, f.pid = true, 4242
		if f.bootstrapNoPID {
			f.pid = 0
		}
		return nil, nil
	}
	return nil, errors.New("unexpected launchctl " + args[0])
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for ; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	return s
}

func (f *fakeLaunchctl) verbs() []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, strings.SplitN(c, " ", 2)[0])
	}
	return out
}

const testExe = "/Users/test/.local/bin/citadel"

// newTestAgent is an agent over a temp home, a fake launchctl and an
// executable this user owns.
func newTestAgent(t *testing.T) (*LaunchAgent, *fakeLaunchctl) {
	t.Helper()
	f := &fakeLaunchctl{}
	return &LaunchAgent{
		Home:    t.TempDir(),
		UID:     501,
		Euid:    501,
		Run:     f.run,
		Resolve: func(p string) (string, error) { return p, nil },
		StatExe: func(string) (ExeInfo, error) {
			return ExeInfo{UID: 501, Mode: 0o755}, nil
		},
	}, f
}

func TestLaunchdUserAgentEnableWritesPerUserPlist(t *testing.T) {
	a, f := newTestAgent(t)
	if err := a.Enable(testExe); err != nil {
		t.Fatal(err)
	}
	if got := a.PlistPath(); got != filepath.Join(a.Home, "Library", "LaunchAgents", "ai.aceteam.citadel.plist") {
		t.Fatalf("plist path = %s", got)
	}
	data, err := os.ReadFile(a.PlistPath())
	if err != nil {
		t.Fatal(err)
	}
	plist := string(data)
	for _, want := range []string{
		"<string>ai.aceteam.citadel</string>",
		"<string>" + testExe + "</string>",
		"<string>work</string>",
		"<key>RunAtLoad</key>\n    <true/>",
		"<key>KeepAlive</key>\n    <true/>",
		launchdServicePATH,
		filepath.Join(a.Home, "Library", "Logs", "citadel") + "/citadel.log",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	// No secrets: the plist carries no token or key, and no sudo/daemon bits.
	for _, bad := range []string{"TOKEN", "token", "API_KEY", "UserName", "--"} {
		if strings.Contains(plist, bad) {
			t.Errorf("plist contains %q:\n%s", bad, plist)
		}
	}
	if fi, _ := os.Stat(a.PlistPath()); fi.Mode().Perm()&0o022 != 0 {
		t.Errorf("plist mode %o is group/other writable", fi.Mode().Perm())
	}
	for _, v := range f.verbs() {
		if v != "print" {
			t.Errorf("enable should only look at launchd, calls = %v", f.calls)
		}
	}
}

func TestLaunchdUserAgentReinstallIsIdempotent(t *testing.T) {
	a, f := newTestAgent(t)
	if err := a.Enable(testExe); err != nil {
		t.Fatal(err)
	}
	f.loaded, f.pid = true, 77
	first, _ := os.Stat(a.PlistPath())
	f.calls = nil

	if err := a.Enable(testExe); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Same content: nothing rewritten, and a running worker is not bounced.
	second, _ := os.Stat(a.PlistPath())
	if !second.ModTime().Equal(first.ModTime()) {
		t.Error("an identical plist was rewritten")
	}
	for _, v := range f.verbs() {
		if v == "bootout" || v == "bootstrap" || v == "load" || v == "unload" {
			t.Errorf("a healthy worker was touched: %v", f.calls)
		}
	}
	if f.pid != 77 {
		t.Errorf("the running worker was replaced (pid %d)", f.pid)
	}
}

func TestLaunchdUserAgentChangedPlistBootsOutTheOldJob(t *testing.T) {
	a, f := newTestAgent(t)
	// An earlier `citadel init` agent for another binary path is adopted.
	old := a.render("/opt/homebrew/Cellar/citadel/1.0/bin/citadel")
	if err := os.MkdirAll(filepath.Dir(a.PlistPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.PlistPath(), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	f.loaded, f.pid = true, 9
	if err := a.Enable(testExe); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(a.PlistPath())
	if !strings.Contains(string(data), testExe) || strings.Contains(string(data), "Cellar") {
		t.Errorf("plist not replaced:\n%s", data)
	}
	if f.loaded {
		t.Error("the job loaded from the old plist was not booted out")
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.loaded || f.pid == 0 {
		t.Error("Start did not load the new plist")
	}
}

func TestLaunchdUserAgentRefusesRootAndUntrustedBinaries(t *testing.T) {
	a, f := newTestAgent(t)
	a.Euid = 0
	for name, err := range map[string]error{"enable": a.Enable(testExe), "start": a.Start(context.Background())} {
		if err == nil || !strings.Contains(err.Error(), "not as root") {
			t.Errorf("%s as root: %v", name, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("root reached launchctl: %v", f.calls)
	}
	if _, err := os.Stat(filepath.Join(a.Home, "Library")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("root wrote under the home directory")
	}

	cases := map[string]struct {
		info ExeInfo
		err  error
		exe  string
		want string
	}{
		"other owner":    {info: ExeInfo{UID: 0, Mode: 0o755}, exe: testExe, want: "owned by another user"},
		"group writable": {info: ExeInfo{UID: 501, Mode: 0o775}, exe: testExe, want: "can be changed by other users"},
		"world writable": {info: ExeInfo{UID: 501, Mode: 0o757}, exe: testExe, want: "can be changed by other users"},
		"directory":      {info: ExeInfo{UID: 501, Mode: fs.ModeDir | 0o755}, exe: testExe, want: "not a regular file"},
		"stat fails":     {err: errors.New("denied"), exe: testExe, want: "could not check"},
		"relative":       {info: ExeInfo{UID: 501, Mode: 0o755}, exe: "bin/citadel", want: "not an absolute path"},
	}
	for name, c := range cases {
		b, _ := newTestAgent(t)
		b.StatExe = func(string) (ExeInfo, error) { return c.info, c.err }
		err := b.Enable(c.exe)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
		if _, serr := os.Stat(b.PlistPath()); !errors.Is(serr, fs.ErrNotExist) {
			t.Errorf("%s: a plist was written", name)
		}
	}
}

func TestLaunchdUserAgentStart(t *testing.T) {
	t.Run("loads the plist and verifies the job", func(t *testing.T) {
		a, f := newTestAgent(t)
		if err := a.Enable(testExe); err != nil {
			t.Fatal(err)
		}
		f.calls = nil
		if err := a.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := "bootstrap gui/501 " + a.PlistPath()
		if !contains(f.calls, want) {
			t.Errorf("calls = %v, want %q", f.calls, want)
		}
	})
	t.Run("a failed bootstrap fails closed, with no legacy load", func(t *testing.T) {
		a, f := newTestAgent(t)
		_ = a.Enable(testExe)
		f.bootstrapFails = true
		err := a.Start(context.Background())
		if err == nil || !strings.Contains(err.Error(), "desktop session") {
			t.Fatalf("err = %v", err)
		}
		if contains(f.verbs(), "load") || f.loaded {
			t.Errorf("calls = %v loaded = %v", f.calls, f.loaded)
		}
	})
	t.Run("a worker that exits at once is a failed start", func(t *testing.T) {
		defer func(w, p time.Duration) { StartWait, StartPoll = w, p }(StartWait, StartPoll)
		StartWait, StartPoll = 30*time.Millisecond, 5*time.Millisecond
		a, f := newTestAgent(t)
		_ = a.Enable(testExe)
		f.bootstrapNoPID = true
		err := a.Start(context.Background())
		if err == nil || !strings.Contains(err.Error(), "not running") || !strings.Contains(err.Error(), "citadel-error.log") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("the caller's context bounds the wait", func(t *testing.T) {
		defer func(w, p time.Duration) { StartWait, StartPoll = w, p }(StartWait, StartPoll)
		StartWait, StartPoll = time.Minute, 5*time.Millisecond
		a, f := newTestAgent(t)
		_ = a.Enable(testExe)
		f.bootstrapNoPID = true
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		start := time.Now()
		if err := a.Start(ctx); err == nil || time.Since(start) > 5*time.Second {
			t.Fatalf("err = %v after %v", err, time.Since(start))
		}
	})
	t.Run("a running worker that is switched off in Login Items is refused", func(t *testing.T) {
		a, f := newTestAgent(t)
		_ = a.Enable(testExe)
		f.loaded, f.pid, f.disabled = true, 8, true
		err := a.Start(context.Background())
		if err == nil || !strings.Contains(err.Error(), "Login Items") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("the context reaches every launchctl call", func(t *testing.T) {
		a, _ := newTestAgent(t)
		_ = a.Enable(testExe)
		type key struct{}
		ctx := context.WithValue(context.Background(), key{}, "x")
		seen := 0
		inner := a.Run
		a.Run = func(c context.Context, args ...string) ([]byte, error) {
			if c.Value(key{}) == nil {
				t.Errorf("launchctl %v ran without the caller's context", args)
			}
			seen++
			return inner(c, args...)
		}
		if err := a.Start(ctx); err != nil {
			t.Fatal(err)
		}
		if seen == 0 {
			t.Fatal("no launchctl calls")
		}
	})
	t.Run("a Login Items switch-off is named", func(t *testing.T) {
		a, f := newTestAgent(t)
		_ = a.Enable(testExe)
		f.disabled = true
		err := a.Start(context.Background())
		if err == nil || !strings.Contains(err.Error(), "Login Items") {
			t.Fatalf("err = %v", err)
		}
		if contains(f.calls, "bootstrap gui/501 "+a.PlistPath()) {
			t.Error("bootstrapped a disabled job")
		}
	})
	t.Run("no plist is refused", func(t *testing.T) {
		a, _ := newTestAgent(t)
		if err := a.Start(context.Background()); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("an oversize log is emptied", func(t *testing.T) {
		a, _ := newTestAgent(t)
		_ = a.Enable(testExe)
		big := filepath.Join(a.logDir(), "citadel.log")
		if err := os.WriteFile(big, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(big, LaunchAgentLogMaxBytes+1); err != nil {
			t.Fatal(err)
		}
		if err := a.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if fi, _ := os.Stat(big); fi.Size() != 0 {
			t.Errorf("log size = %d", fi.Size())
		}
	})
}

func TestLaunchdUserAgentStop(t *testing.T) {
	t.Run("not loaded is success without a bootout", func(t *testing.T) {
		a, f := newTestAgent(t)
		if err := a.Stop(); err != nil {
			t.Fatal(err)
		}
		if contains(f.verbs(), "bootout") {
			t.Errorf("calls = %v", f.calls)
		}
	})
	t.Run("boots out by service target", func(t *testing.T) {
		a, f := newTestAgent(t)
		f.loaded, f.pid = true, 5
		if err := a.Stop(); err != nil || f.loaded {
			t.Fatalf("err = %v loaded = %v", err, f.loaded)
		}
		if !contains(f.calls, "bootout gui/501/ai.aceteam.citadel") {
			t.Errorf("calls = %v", f.calls)
		}
	})
	t.Run("a failed bootout that leaves the job loaded is an error", func(t *testing.T) {
		a, f := newTestAgent(t)
		f.loaded, f.pid, f.bootoutFails = true, 5, true
		err := a.Stop()
		if err == nil || !strings.Contains(err.Error(), "could not stop") {
			t.Fatalf("err = %v", err)
		}
		if contains(f.verbs(), "unload") {
			t.Errorf("the legacy unload must not be used: %v", f.calls)
		}
		if !strings.Contains(err.Error(), "desktop session") {
			t.Errorf("no guidance: %v", err)
		}
	})
	t.Run("a bootout that errors but removed the job is success", func(t *testing.T) {
		a, f := newTestAgent(t)
		f.loaded, f.bootoutLies = true, true
		if err := a.Stop(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a broken launchctl is never read as not loaded", func(t *testing.T) {
		a, f := newTestAgent(t)
		f.hardErr = errors.New("launchctl: executable file not found")
		if err := a.Stop(); err == nil {
			t.Fatal("expected an error")
		}
	})
}

// TestLaunchdUserAgentStopOverSSHStyleFailureIsNotStopped: where gui/<uid> is
// unreachable, print exits with something other than "service not found". That
// is never read as "stopped".
func TestLaunchdUserAgentStopOverSSHStyleFailureIsNotStopped(t *testing.T) {
	a, f := newTestAgent(t)
	f.printExit = 112 // no such domain
	err := a.Stop()
	if err == nil || !strings.Contains(err.Error(), "launchctl print") {
		t.Fatalf("err = %v", err)
	}
	f.printExit = 113
	if err := a.Stop(); err != nil {
		t.Fatalf("service-not-found is the only absent signal: %v", err)
	}
	f.printExit = 112
	if st := a.State(testExe); st.Err == nil {
		t.Error("State must report an unreadable domain")
	}
}

func TestLaunchdUserAgentRefusesWritableParentDirectories(t *testing.T) {
	a, _ := newTestAgent(t)
	dirMode := os.FileMode(0o755)
	dirUID := 501
	a.StatExe = func(p string) (ExeInfo, error) {
		if p == filepath.Dir(testExe) {
			return ExeInfo{UID: dirUID, Mode: fs.ModeDir | dirMode}, nil
		}
		return ExeInfo{UID: 501, Mode: 0o755}, nil
	}
	if err := a.Enable(testExe); err != nil {
		t.Fatalf("a user-owned 755 folder (Homebrew on Apple Silicon): %v", err)
	}
	dirMode = 0o775
	if err := a.Enable(testExe); err == nil || !strings.Contains(err.Error(), "folder") {
		t.Fatalf("group-writable folder: %v", err)
	}
	dirMode, dirUID = 0o755, 502
	if err := a.Enable(testExe); err == nil || !strings.Contains(err.Error(), "owned by another user") {
		t.Fatalf("foreign folder: %v", err)
	}
	dirUID = 0 // root-owned (for example /usr/local/bin) is fine
	if err := a.Enable(testExe); err != nil {
		t.Fatalf("root-owned folder: %v", err)
	}
}

func TestLaunchdTruncateIfOversizeSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "log")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("no symlinks")
	}
	truncateIfOversize(link, 4)
	if b, _ := os.ReadFile(target); len(b) != 10 {
		t.Error("a symlinked log was followed and truncated")
	}
	truncateIfOversize(target, 4)
	if b, _ := os.ReadFile(target); len(b) != 0 {
		t.Error("an oversize regular file was not emptied")
	}
}

func TestLaunchdUserAgentRemoveAndState(t *testing.T) {
	a, f := newTestAgent(t)
	if err := a.Remove(); err != nil {
		t.Fatalf("removing an absent plist: %v", err)
	}
	st := a.State(testExe)
	if st.PlistPresent || st.Loaded || st.Err != nil {
		t.Fatalf("empty state = %+v", st)
	}
	if err := a.Enable(testExe); err != nil {
		t.Fatal(err)
	}
	f.loaded, f.pid, f.disabled = true, 31, true
	if err := os.WriteFile(filepath.Join(a.logDir(), "citadel.log"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	st = a.State(testExe)
	if !st.PlistPresent || !st.Loaded || st.PID != 31 || !st.Disabled || !st.PointsAtThis || st.Exe != testExe || st.LogBytes != 5 {
		t.Fatalf("state = %+v", st)
	}
	if a.State("/somewhere/else/citadel").PointsAtThis {
		t.Error("PointsAtThis for another binary")
	}
	if err := a.Remove(); err != nil {
		t.Fatal(err)
	}
	if a.State(testExe).PlistPresent {
		t.Error("plist still present after Remove")
	}
}

// TestLaunchdUserAgentWritesOnlyUnderItsHome: every file the agent creates is
// inside the injected home.
func TestLaunchdUserAgentWritesOnlyUnderItsHome(t *testing.T) {
	a, _ := newTestAgent(t)
	if err := a.Enable(testExe); err != nil {
		t.Fatal(err)
	}
	var got []string
	_ = filepath.WalkDir(a.Home, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(a.Home, p)
			got = append(got, rel)
		}
		return nil
	})
	if len(got) != 1 || got[0] != filepath.Join("Library", "LaunchAgents", "ai.aceteam.citadel.plist") {
		t.Fatalf("files = %v", got)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// dirTable builds a StatExe from per-path facts; a path not listed is a
// user-owned 755 folder or file.
func dirTable(facts map[string]ExeInfo) func(string) (ExeInfo, error) {
	return func(p string) (ExeInfo, error) {
		if i, ok := facts[p]; ok {
			return i, nil
		}
		if p == testExe {
			return ExeInfo{UID: 501, Mode: 0o755}, nil
		}
		return ExeInfo{UID: 501, Mode: fs.ModeDir | 0o755}, nil
	}
}

func TestLaunchdUserAgentChecksEveryAncestorDirectory(t *testing.T) {
	const brewLink = "/opt/homebrew/bin/citadel"
	const brewReal = "/opt/homebrew/Cellar/citadel/1.0/bin/citadel"
	root := ExeInfo{UID: 0, Mode: fs.ModeDir | 0o755}
	sticky := ExeInfo{UID: 0, Mode: fs.ModeDir | fs.ModeSticky | 0o777}
	cases := []struct {
		name    string
		exe     string
		resolve string
		facts   map[string]ExeInfo
		want    string // "" passes
	}{
		{"user bin, root ancestors", testExe, testExe, map[string]ExeInfo{"/": root, "/Users": root}, ""},
		{"homebrew apple silicon: user-owned 755 prefix, resolved into the cellar", brewLink, brewReal,
			map[string]ExeInfo{"/": root, "/opt": root}, ""},
		{"writable grandparent of the path", testExe, testExe,
			map[string]ExeInfo{"/Users/test": {UID: 501, Mode: fs.ModeDir | 0o777}}, "/Users/test"},
		{"writable root-level ancestor", testExe, testExe,
			map[string]ExeInfo{"/Users": {UID: 0, Mode: fs.ModeDir | 0o775}}, "/Users"},
		{"group-writable ancestor of the resolved target only", brewLink, brewReal,
			map[string]ExeInfo{"/opt/homebrew/Cellar": {UID: 501, Mode: fs.ModeDir | 0o775}}, "/opt/homebrew/Cellar"},
		{"writable ancestor of the symlink path only", brewLink, brewReal,
			map[string]ExeInfo{"/opt/homebrew": {UID: 501, Mode: fs.ModeDir | 0o770}}, "/opt/homebrew"},
		{"ancestor owned by another user", testExe, testExe,
			map[string]ExeInfo{"/Users": {UID: 502, Mode: fs.ModeDir | 0o755}}, "owned by another user"},
		{"sticky root-owned ancestor (like /tmp) is allowed", "/tmp/x/citadel", "/tmp/x/citadel",
			map[string]ExeInfo{"/tmp": sticky, "/tmp/x/citadel": {UID: 501, Mode: 0o755}}, ""},
		{"sticky ancestor owned by another user is refused", "/tmp/x/citadel", "/tmp/x/citadel",
			map[string]ExeInfo{"/tmp": {UID: 502, Mode: fs.ModeDir | fs.ModeSticky | 0o777}, "/tmp/x/citadel": {UID: 501, Mode: 0o755}}, "owned by another user"},
		{"resolved file owned by another user", brewLink, brewReal,
			map[string]ExeInfo{brewReal: {UID: 0, Mode: 0o755}}, "what"},
	}
	for _, c := range cases {
		a, _ := newTestAgent(t)
		facts := c.facts
		if _, ok := facts[c.exe]; !ok {
			facts[c.exe] = ExeInfo{UID: 501, Mode: 0o755}
		}
		if _, ok := facts[c.resolve]; !ok {
			facts[c.resolve] = ExeInfo{UID: 501, Mode: 0o755}
		}
		a.StatExe = dirTable(facts)
		a.Resolve = func(string) (string, error) { return c.resolve, nil }
		err := a.checkExe(c.exe)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected refusal: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	a, _ := newTestAgent(t)
	a.Resolve = func(string) (string, error) { return "", errors.New("dangling") }
	if err := a.checkExe(testExe); err == nil || !strings.Contains(err.Error(), "could not resolve") {
		t.Errorf("an unresolvable path must be refused: %v", err)
	}
}

func TestLaunchdRunsLaunchctlByAbsolutePath(t *testing.T) {
	cmd := launchctlCommand(context.Background(), "print", "gui/501/x")
	if cmd.Path != "/bin/launchctl" || cmd.Args[0] != "/bin/launchctl" {
		t.Fatalf("path = %q args = %q", cmd.Path, cmd.Args)
	}
	if launchctlPath != "/bin/launchctl" || !filepath.IsAbs(launchctlPath) {
		t.Fatalf("launchctlPath = %q", launchctlPath)
	}
}

func TestLaunchdEnableLeavesNoPlistWhenLaunchdIsUnreachable(t *testing.T) {
	a, f := newTestAgent(t)
	f.printExit = 112
	err := a.Enable(testExe)
	if err == nil || !strings.Contains(err.Error(), "desktop session") {
		t.Fatalf("err = %v", err)
	}
	if _, serr := os.Stat(a.PlistPath()); !errors.Is(serr, fs.ErrNotExist) {
		t.Error("a RunAtLoad plist was left behind")
	}
	for _, v := range f.verbs() {
		if v != "print" {
			t.Errorf("unexpected launchctl %v", f.calls)
		}
	}
}

func TestLaunchdEnableRollsBackWhenTheOldJobCannotBeBootedOut(t *testing.T) {
	// A new plist is removed.
	a, f := newTestAgent(t)
	f.loaded, f.bootoutFails = true, true
	// pre-check passes (print ok), stop fails after the write.
	if err := a.Enable(testExe); err == nil {
		t.Fatal("expected an error")
	}
	if _, serr := os.Stat(a.PlistPath()); !errors.Is(serr, fs.ErrNotExist) {
		t.Error("the new plist was kept")
	}

	// A replaced plist is restored.
	b, g := newTestAgent(t)
	old := b.render("/old/citadel")
	if err := os.MkdirAll(filepath.Dir(b.PlistPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.PlistPath(), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	g.loaded, g.bootoutFails = true, true
	if err := b.Enable(testExe); err == nil {
		t.Fatal("expected an error")
	}
	if got, _ := os.ReadFile(b.PlistPath()); string(got) != old {
		t.Errorf("the previous plist was not restored:\n%s", got)
	}
}
