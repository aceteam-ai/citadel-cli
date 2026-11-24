package service

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLaunchctl is a launchctl runner over an in-memory job table. It records
// every call so tests can assert the order launchd was driven in.
type fakeLaunchctl struct {
	calls []string

	loaded   bool
	pid      int
	disabled bool

	bootoutFails   bool // bootout and unload both fail and the job stays loaded
	bootoutLies    bool // bootout reports failure but the job is gone
	bootstrapFails bool // bootstrap fails (load still works)
	loadFails      bool
	hardErr        error // returned for every call: launchctl itself is unusable
}

func (f *fakeLaunchctl) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if f.hardErr != nil {
		return nil, f.hardErr
	}
	exit := &LaunchctlExit{Code: 113, Output: "Could not find service"}
	switch args[0] {
	case "print":
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
	case "bootout", "unload":
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
		return nil, nil
	case "load":
		if f.loadFails {
			return nil, exit
		}
		f.loaded, f.pid = true, 4242
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
		Home: t.TempDir(),
		UID:  501,
		Euid: 501,
		Run:  f.run,
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
	if len(f.calls) != 1 || !strings.HasPrefix(f.calls[0], "print ") {
		t.Errorf("enable should only look at launchd, calls = %v", f.calls)
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
	if err := a.Start(); err != nil {
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
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	if !f.loaded || f.pid == 0 {
		t.Error("Start did not load the new plist")
	}
}

func TestLaunchdUserAgentRefusesRootAndUntrustedBinaries(t *testing.T) {
	a, f := newTestAgent(t)
	a.Euid = 0
	for name, err := range map[string]error{"enable": a.Enable(testExe), "start": a.Start()} {
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
		if err := a.Start(); err != nil {
			t.Fatal(err)
		}
		want := "bootstrap gui/501 " + a.PlistPath()
		if !contains(f.calls, want) {
			t.Errorf("calls = %v, want %q", f.calls, want)
		}
	})
	t.Run("falls back to load without a GUI session", func(t *testing.T) {
		a, f := newTestAgent(t)
		_ = a.Enable(testExe)
		f.bootstrapFails = true
		if err := a.Start(); err != nil || !f.loaded {
			t.Fatalf("err = %v loaded = %v", err, f.loaded)
		}
	})
	t.Run("both verbs failing is an error", func(t *testing.T) {
		a, f := newTestAgent(t)
		_ = a.Enable(testExe)
		f.bootstrapFails, f.loadFails = true, true
		if err := a.Start(); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("a Login Items switch-off is named", func(t *testing.T) {
		a, f := newTestAgent(t)
		_ = a.Enable(testExe)
		f.disabled = true
		err := a.Start()
		if err == nil || !strings.Contains(err.Error(), "Login Items") {
			t.Fatalf("err = %v", err)
		}
		if contains(f.calls, "bootstrap gui/501 "+a.PlistPath()) {
			t.Error("bootstrapped a disabled job")
		}
	})
	t.Run("no plist is refused", func(t *testing.T) {
		a, _ := newTestAgent(t)
		if err := a.Start(); err == nil {
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
		if err := a.Start(); err != nil {
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
		if !contains(f.verbs(), "unload") {
			t.Errorf("the legacy unload was not tried: %v", f.calls)
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
