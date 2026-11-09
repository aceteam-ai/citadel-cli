// cmd/local_doctor.go
//
// `citadel local doctor`: one line per local-mode prerequisite, each with a fix
// (aceteam#13503 S6, design section 6). It makes no network calls.
package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/localaudit"
	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/spf13/cobra"
)

type localDoctorDeps struct {
	goos     string
	runner   string
	discover func() (platform.BrowserInfo, error)
	// launchTest returns the launched browser's kind and its /json/version
	// "Browser" string (e.g. "HeadlessChrome/131.0.0.0").
	launchTest func() (kind, version string, err error)
	bindTest   func() error
	vault      func() (bool, error)
	owner      func() error
	audit      func() (localaudit.VerifyResult, error)
	harnesses  []harnessConfigStatus
	// exe is how fix lines refer to citadel (the full path, quoted if needed).
	exe string
}

func realLocalDoctorDeps(skipLaunch bool) localDoctorDeps {
	nodeDir := network.GetNodeConfigDir()
	d := localDoctorDeps{
		goos:     runtime.GOOS,
		runner:   platform.CobrowseRunnerName(),
		discover: platform.DiscoverCobrowseBrowser,
		bindTest: func() error {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return err
			}
			return ln.Close()
		},
		vault: func() (bool, error) { return nodevault.Open(nodeDir).Configured() },
		owner: func() error {
			o, err := localcobrowse.AcquireOwner(nodeDir)
			if err != nil {
				return err
			}
			o.Release()
			return nil
		},
		audit: func() (localaudit.VerifyResult, error) {
			return localaudit.Verify(filepath.Join(nodeDir, localaudit.DirName))
		},
		harnesses: detectHarnessConfigs(systemHarnessEnv()),
		exe:       "citadel",
	}
	if exe, err := citadelExecutable(); err == nil {
		d.exe = quoteExe(exe)
	}
	if !skipLaunch {
		d.launchTest = func() (string, string, error) {
			m := platform.NewCobrowseSessionManager(platform.DefaultCobrowseSessionsBaseDir(), 1)
			m.SetLaunchOptions(platform.LaunchOptions{Quiet: true})
			st, err := m.StartSession("about:blank")
			if err != nil {
				return "", "", err
			}
			version := cdpBrowserVersion(st.DebugPort)
			return st.Browser, version, m.Stop(st.ID)
		}
	}
	return d
}

var localDoctorSkipLaunch bool

var localDoctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check that local co-browse can run on this computer",
	Long: `Checks each local-mode prerequisite and prints a fix for anything missing:
the browser Citadel will use, a hidden (headless) test launch, the local viewer
port, the passphrase vault, other agent apps, the audit log, and which agent
app configs already point at citadel. It makes no network calls.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runLocalDoctor(realLocalDoctorDeps(localDoctorSkipLaunch), cmd.OutOrStdout())
	},
}

func init() {
	localDoctorCmd.Flags().BoolVar(&localDoctorSkipLaunch, "no-launch", false, "skip the test browser launch")
	localCmd.AddCommand(localDoctorCmd)
}

var errLocalDoctorFailed = errors.New("local mode is not ready; see the lines marked FAIL above")

func runLocalDoctor(d localDoctorDeps, out io.Writer) error {
	failed := false
	line := func(status, what, detail string) {
		fmt.Fprintf(out, "[%-4s] %-14s %s\n", status, what, detail)
	}
	ok := func(what, detail string) { line("ok", what, detail) }
	warn := func(what, detail string) { line("note", what, detail) }
	fail := func(what, detail string) { failed = true; line("FAIL", what, detail) }

	info, err := d.discover()
	if err != nil {
		fail("Browser", err.Error())
	} else {
		ok("Browser", fmt.Sprintf("%s at %s", browserTitle(info.Kind), info.Path))
	}

	switch {
	case d.launchTest == nil:
		warn("Test launch", "skipped (--no-launch)")
	case err != nil:
		fail("Test launch", "skipped: no browser")
	default:
		if kind, version, lerr := d.launchTest(); lerr != nil {
			fix := "run `citadel local doctor` again; if it repeats, restart the computer"
			if errors.Is(lerr, platform.ErrBrowserAutomationBlocked) {
				fix = "use a personal (per-user) Chrome install or a computer without that policy"
			}
			if d.runner == "xvfb" {
				fix = "install Xvfb (e.g. apt-get install xvfb); a headless Linux runner is planned"
			}
			fail("Test launch", fmt.Sprintf("%v. Fix: %s", lerr, fix))
		} else if major := browserMajor(version); major > 0 && major < minHeadlessMajor {
			fail("Test launch", fmt.Sprintf("%s %s is too old for hidden sessions (needs %d or newer). Fix: update the browser (Edge: Settings, About Microsoft Edge)", browserTitle(kind), version, minHeadlessMajor))
		} else {
			v := version
			if v == "" {
				v = "version unknown"
			}
			ok("Test launch", fmt.Sprintf("%s (%s) started hidden (%s runner) and stopped cleanly", browserTitle(kind), v, d.runner))
		}
	}

	if err := d.bindTest(); err != nil {
		fail("Viewer port", fmt.Sprintf("cannot listen on 127.0.0.1 (%v). Fix: check security software that blocks local connections", err))
	} else {
		ok("Viewer port", "127.0.0.1 is available (the viewer never listens on your network)")
	}

	if set, err := d.vault(); err != nil {
		warn("Passphrase", fmt.Sprintf("vault state unreadable: %v", err))
	} else if set {
		ok("Passphrase", "a vault passphrase is set")
	} else {
		ok("Passphrase", "not set; only needed later to remember logins on this computer")
	}

	var oe *localcobrowse.OwnerError
	if err := d.owner(); errors.As(err, &oe) {
		warn("Agent apps", oe.Error())
	} else if err != nil {
		fail("Agent apps", fmt.Sprintf("cannot use the co-browse owner lock: %v", err))
	} else {
		ok("Agent apps", "no other agent app is using co-browse right now")
	}

	if res, err := d.audit(); err != nil {
		fail("Audit log", fmt.Sprintf("check failed: %v. The log may have been edited; keep a copy and investigate", err))
	} else if res.Records == 0 {
		ok("Audit log", "empty (records appear once an agent uses co-browse)")
	} else {
		ok("Audit log", fmt.Sprintf("%d records, chain intact", res.Records))
	}

	pointed := 0
	for _, h := range d.harnesses {
		switch {
		case h.Points:
			pointed++
			ok("Harness", fmt.Sprintf("%s: %s runs %s local mcp", h.Harness, h.Path, h.Command))
		case h.Found:
			warn("Harness", fmt.Sprintf("%s: %s has no working citadel entry. Fix: %s local config %s --write", h.Harness, h.Path, d.exe, h.Harness))
		}
	}
	if pointed == 0 {
		warn("Harness", fmt.Sprintf("no agent app is configured yet. Fix: %s local config copilot (or claude-desktop, codex, opencode)", d.exe))
	}

	ok("Network", "Citadel makes no calls of its own (no account, no update check, no telemetry). The co-browse browser connects only to public addresses, through Citadel's local guard, with background updates off")

	if failed {
		return errLocalDoctorFailed
	}
	return nil
}

// minHeadlessMajor is the first Chromium release with --headless=new.
const minHeadlessMajor = 109

// cdpBrowserVersion reads the "Browser" field of the loopback CDP
// /json/version endpoint, or "" if it cannot.
func cdpBrowserVersion(port int) string {
	if port <= 0 {
		return ""
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var v struct {
		Browser string `json:"Browser"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&v) != nil {
		return ""
	}
	return v.Browser
}

// browserMajor parses the major version from "Name/131.0.1.2", or 0.
func browserMajor(v string) int {
	i := strings.LastIndex(v, "/")
	if i < 0 {
		return 0
	}
	n, _ := strconv.Atoi(strings.SplitN(v[i+1:], ".", 2)[0])
	return n
}

func browserTitle(kind string) string {
	switch kind {
	case "edge":
		return "Microsoft Edge"
	case "chrome":
		return "Google Chrome"
	case "":
		return "Browser"
	default:
		return "Chromium"
	}
}
