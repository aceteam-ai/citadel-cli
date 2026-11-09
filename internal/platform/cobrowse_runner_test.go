package platform

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestBuildChromeArgs_LinuxArgsUnchanged pins the exact headed (Xvfb) argument
// list so the Windows headless options cannot change the Linux launch.
func TestBuildChromeArgs_LinuxArgsUnchanged(t *testing.T) {
	got := buildChromeArgs(cobrowseLaunchOptions{
		debugPort:  9333,
		profileDir: "/p",
		startURL:   "http://example.test/",
		stealth:    true,
		softwareGL: true,
	})
	want := []string{
		"--remote-debugging-port=9333",
		"--remote-debugging-address=127.0.0.1",
		"--user-data-dir=/p",
		"--no-first-run",
		"--no-default-browser-check",
		"--start-maximized",
		"--disable-blink-features=AutomationControlled",
		"--lang=en-US",
		"--disable-features=Translate",
		"--disable-gpu",
		"http://example.test/",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("headed args changed:\n got %v\nwant %v", got, want)
	}
}

func TestBuildChromeArgs_Headless(t *testing.T) {
	args := buildChromeArgs(cobrowseLaunchOptions{
		debugPort:  9444,
		profileDir: `C:\s\cb-1`,
		stealth:    true,
		headless:   true,
		windowSize: "1280,720",
	})
	for _, want := range []string{
		"--headless=new",
		"--window-size=1280,720",
		"--remote-debugging-address=127.0.0.1",
		"--disable-blink-features=AutomationControlled",
	} {
		if !containsArg(args, want) {
			t.Errorf("missing %s in %v", want, args)
		}
	}
	if containsArg(args, "--start-maximized") {
		t.Errorf("headless launch must not request a maximized window: %v", args)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "--remote-allow-origins") {
			t.Errorf("must never allow web origins on the debug socket: %v", args)
		}
	}
}

type fakeWinProbe struct {
	env     map[string]string
	files   map[string]bool
	app     map[string]string // "HKCU|chrome.exe" -> path
	onPath  map[string]string
	lookups []string
}

func (f *fakeWinProbe) probe() windowsBrowserProbe {
	return windowsBrowserProbe{
		getenv: func(k string) string { return f.env[k] },
		isFile: func(p string) bool { return f.files[p] },
		appPath: func(hive, exe string) string {
			return f.app[hive+"|"+exe]
		},
		lookPath: func(name string) (string, error) {
			f.lookups = append(f.lookups, name)
			if p, ok := f.onPath[name]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
	}
}

func baseWinEnv() map[string]string {
	return map[string]string{
		"LOCALAPPDATA":      `C:\Users\u\AppData\Local`,
		"ProgramFiles":      `C:\Program Files`,
		"ProgramFiles(x86)": `C:\Program Files (x86)`,
	}
}

func TestDiscoverWindowsBrowser_Order(t *testing.T) {
	userChrome := `C:\Users\u\AppData\Local\Google\Chrome\Application\chrome.exe`
	machineChrome := `C:\Program Files\Google\Chrome\Application\chrome.exe`
	machineChrome86 := `C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`
	appPathChrome := `D:\Apps\Chrome\chrome.exe`
	appPathEdge := `D:\Apps\Edge\msedge.exe`
	edge86 := `C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`
	edge := `C:\Program Files\Microsoft\Edge\Application\msedge.exe`

	cases := []struct {
		name     string
		files    []string
		app      map[string]string
		onPath   map[string]string
		wantPath string
		wantKind string
	}{
		{"per-user chrome first", []string{userChrome, machineChrome, edge86}, nil, nil, userChrome, "chrome"},
		{"machine chrome", []string{machineChrome, edge86}, nil, nil, machineChrome, "chrome"},
		{"machine chrome x86", []string{machineChrome86, edge86}, nil, nil, machineChrome86, "chrome"},
		{"app paths chrome before edge dir", []string{appPathChrome, edge86},
			map[string]string{"HKLM|chrome.exe": `"` + appPathChrome + `"`}, nil, appPathChrome, "chrome"},
		{"app paths HKCU before HKLM", []string{appPathEdge, appPathChrome},
			map[string]string{"HKCU|msedge.exe": appPathEdge, "HKLM|msedge.exe": appPathChrome}, nil, appPathEdge, "edge"},
		{"edge x86 install", []string{edge86, edge}, nil, nil, edge86, "edge"},
		{"edge program files", []string{edge}, nil, nil, edge, "edge"},
		{"PATH last", nil, nil, map[string]string{"msedge.exe": `E:\bin\msedge.exe`}, `E:\bin\msedge.exe`, "edge"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeWinProbe{env: baseWinEnv(), files: map[string]bool{}, app: tc.app, onPath: tc.onPath}
			for _, p := range tc.files {
				f.files[p] = true
			}
			info, err := discoverWindowsBrowser(f.probe())
			if err != nil {
				t.Fatalf("discover: %v", err)
			}
			if info.Path != tc.wantPath || info.Kind != tc.wantKind {
				t.Fatalf("got %+v, want path=%s kind=%s", info, tc.wantPath, tc.wantKind)
			}
		})
	}
}

func TestDiscoverWindowsBrowser_OverrideIsExact(t *testing.T) {
	edge := `C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`
	f := &fakeWinProbe{env: baseWinEnv(), files: map[string]bool{edge: true}}
	f.env[EnvCobrowseBrowser] = `C:\custom\chrome.exe`
	_, err := discoverWindowsBrowser(f.probe())
	if !errors.Is(err, ErrNoBrowser) {
		t.Fatalf("missing override must fail, not fall back to Edge; got %v", err)
	}
	f.files[`C:\custom\chrome.exe`] = true
	info, err := discoverWindowsBrowser(f.probe())
	if err != nil || info.Path != `C:\custom\chrome.exe` || info.Kind != "chrome" {
		t.Fatalf("override: got %+v err %v", info, err)
	}
}

func TestDiscoverWindowsBrowser_NoneListsSearched(t *testing.T) {
	f := &fakeWinProbe{env: baseWinEnv(), files: map[string]bool{}}
	_, err := discoverWindowsBrowser(f.probe())
	if !errors.Is(err, ErrNoBrowser) {
		t.Fatalf("want ErrNoBrowser, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"Install Microsoft Edge or Google Chrome", `Microsoft\Edge\Application\msedge.exe`, "PATH:chrome.exe", EnvCobrowseBrowser} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q: %s", want, msg)
		}
	}
}

func TestBrowserKindForPath(t *testing.T) {
	for in, want := range map[string]string{
		`C:\x\MSEDGE.EXE`:              "edge",
		`C:\x\chrome.exe`:              "chrome",
		"/usr/bin/google-chrome":       "chrome",
		"chromium-browser":             "chromium",
		"/opt/thing/chromium":          "chromium",
		`C:\Program Files\x\brave.exe`: "chromium",
	} {
		if got := browserKindForPath(in); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}

func TestSessionPidfile_OwnerStartLine(t *testing.T) {
	dir := t.TempDir()
	writeSessionPidfile(dir, 1, 2, 3, 0)
	data, _ := os.ReadFile(filepath.Join(dir, sessionPidfileName))
	if string(data) != "1\n2\n3\n" {
		t.Fatalf("zero owner start must keep the three-line format, got %q", data)
	}
	if got := readSessionOwnerStart(dir); got != 0 {
		t.Fatalf("absent owner start: got %d", got)
	}
	writeSessionPidfile(dir, 1, 2, 0, 133456789012345678)
	if o, b, x := readSessionPidfile(dir); o != 1 || b != 2 || x != 0 {
		t.Fatalf("pids: %d %d %d", o, b, x)
	}
	if got := readSessionOwnerStart(dir); got != 133456789012345678 {
		t.Fatalf("owner start: got %d", got)
	}
}

// TestSessionManager_PrepareBaseDirFailureRefusesLaunch proves a runner that
// rejects the base dir stops the launch before the browser starts.
func TestSessionManager_PrepareBaseDirFailureRefusesLaunch(t *testing.T) {
	prev := activeBrowserRunner
	activeBrowserRunner = refusingRunner{}
	t.Cleanup(func() { activeBrowserRunner = prev })
	called := false
	prevLaunch := launchCobrowseProc
	launchCobrowseProc = func(string, string, LaunchOptions) (*cobrowseProc, error) {
		called = true
		return nil, errors.New("must not launch")
	}
	t.Cleanup(func() { launchCobrowseProc = prevLaunch })

	m := newCobrowseSessionManager(filepath.Join(t.TempDir(), "s"), 2)
	if _, err := m.StartSession(""); err == nil || !strings.Contains(err.Error(), "not private") {
		t.Fatalf("want refusal, got %v", err)
	}
	if called {
		t.Fatal("launch ran despite an untrusted base dir")
	}
	if len(m.List()) != 0 {
		t.Fatal("refused launch must not leave a session slot")
	}
}

type refusingRunner struct{ xvfbRunner }

func (refusingRunner) PrepareBaseDir(string) error { return errors.New("base dir not private") }
func (refusingRunner) SweepOrphans(string)         {}

func TestDeheadlessUserAgent(t *testing.T) {
	cases := map[string]string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/131.0.0.0 Safari/537.36":               "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/130.0.0.0 Safari/537.36 Edg/130.0.0.0": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36 Edg/130.0.0.0",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36":                                 "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36",
	}
	for in, want := range cases {
		if got := deheadlessUserAgent(in); got != want {
			t.Errorf("got %q\nwant %q", got, want)
		}
	}
}

func TestParseDevToolsActivePort(t *testing.T) {
	port, id, err := parseDevToolsActivePort([]byte("51234\n/devtools/browser/0b5d1c2e-aaaa-bbbb-cccc-1234567890ab\n"))
	if err != nil || port != 51234 || id != "0b5d1c2e-aaaa-bbbb-cccc-1234567890ab" {
		t.Fatalf("got %d %q %v", port, id, err)
	}
	for _, bad := range []string{"", "51234\n", "x\n/devtools/browser/a", "0\n/devtools/browser/a", "51234\n/devtools/page/a", "51234\n/devtools/browser/"} {
		if _, _, err := parseDevToolsActivePort([]byte(bad)); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestBuildChromeArgs_ProxyAndQuiet(t *testing.T) {
	args := buildChromeArgs(cobrowseLaunchOptions{debugPort: 0, profileDir: "/p", headless: true, proxyServer: "http://127.0.0.1:4000", quiet: true})
	for _, want := range []string{
		"--remote-debugging-port=0",
		"--proxy-server=http://127.0.0.1:4000",
		"--proxy-bypass-list=<-loopback>",
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
		"--disable-background-networking",
		"--disable-component-update",
	} {
		if !containsArg(args, want) {
			t.Errorf("missing %s in %v", want, args)
		}
	}
}

func TestVerifyCDPBrowserIDRejectsImpostor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"webSocketDebuggerUrl":"ws://127.0.0.1:1/devtools/browser/other-id"}`)
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	if err := verifyCDPBrowserID(port, "our-id"); !errors.Is(err, errCDPImpostor) {
		t.Fatalf("want impostor, got %v", err)
	}
	if err := verifyCDPBrowserID(port, "other-id"); err != nil {
		t.Fatalf("matching id: %v", err)
	}
}
