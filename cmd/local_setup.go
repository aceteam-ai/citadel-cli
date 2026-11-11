// cmd/local_setup.go
//
// `citadel local setup` and the Windows double-click path (aceteam-ai/citadel-cli#1615,
// slice 1): the first-run setup page. internal/localviewer owns the server, its
// guard and every secret; this file owns what the page shows and does: the
// doctor results, the agent apps (cmd.harnesses) and the Connect action, which
// is mergeHarnessConfig, the same merge and backup as `local config --write`.
// The consent is the click on the page. Nothing here makes an outbound call.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/localcobrowse"
	"github.com/aceteam-ai/citadel-cli/internal/localviewer"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/spf13/cobra"
)

// consoleProcessCount is the seam for Explorer launch detection.
var consoleProcessCount = platform.ConsoleProcessCount

// launchedFromExplorer reports whether this process is alone on its console,
// which is what a double-click in Explorer produces (a terminal always adds its
// shell to the console). `start citadel` and `Start-Process citadel` also get
// a console of their own and so also count; that is intended and documented. It is false on every other OS and when there is no
// console to ask.
func launchedFromExplorer() bool {
	n, err := consoleProcessCount()
	return err == nil && n == 1
}

// bareRunLocalSetupFn is the seam the double-click path runs through.
var bareRunLocalSetupFn = runLocalSetupCommand

// runRootCitadel is bare `citadel`. A double-click on Windows has no terminal to
// print to, so it opens the guided setup page instead of the bare dispatcher;
// every other launch is unchanged. The test is a console with no other process
// on it, so `start citadel` or any other new-console launch also opens setup,
// not only an Explorer double-click. It sits here, not in runBareCitadel, which
// is pinned to enrollment dispatch only.
func runRootCitadel(cmd *cobra.Command, args []string) {
	if launchedFromExplorer() {
		if err := bareRunLocalSetupFn(cmd.OutOrStdout()); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return
	}
	runBareCitadel(cmd, args)
}

// harnessInstalled reports whether the app's settings folder or file exists.
// An app that was never started may have neither; it then reads as not found.
func harnessInstalled(h harnessSpec, env harnessEnv) bool {
	p := h.path(env)
	if _, err := os.Stat(p); err == nil {
		return true
	}
	fi, err := os.Stat(filepath.Dir(p))
	return err == nil && fi.IsDir()
}

func setupApps(env harnessEnv) []localviewer.SetupApp {
	status := map[string]harnessConfigStatus{}
	for _, st := range detectHarnessConfigs(env) {
		status[st.Harness] = st
	}
	var out []localviewer.SetupApp
	for _, h := range harnesses {
		app := localviewer.SetupApp{Name: h.name, Title: h.title, Path: h.path(env), Note: h.unverified(env)}
		switch {
		case status[h.name].Points:
			app.State = "connected"
		case harnessInstalled(h, env):
			app.State = "available"
		default:
			app.State = "not_found"
		}
		out = append(out, app)
	}
	return out
}

func harnessNextStep(h harnessSpec) string {
	return fmt.Sprintf("Next: restart %s, then ask it to browse a site.", h.title)
}

// setupConnect is the Connect click. expectedPath is the file the page showed;
// a mismatch means the page is stale, so nothing is written.
func setupConnect(name, expectedPath, exe string, env harnessEnv, now time.Time) (localviewer.SetupConnectResult, error) {
	h, ok := findHarness(name)
	if !ok {
		return localviewer.SetupConnectResult{}, fmt.Errorf("unknown app %q", name)
	}
	path := h.path(env)
	if path != expectedPath {
		return localviewer.SetupConnectResult{}, errors.New("the settings file changed since this page loaded; reload the page and try again")
	}
	if !harnessInstalled(h, env) {
		return localviewer.SetupConnectResult{}, fmt.Errorf("%s was not found on this computer", h.title)
	}
	_, statErr := os.Stat(path)
	existed := statErr == nil
	// The click consents to adding Citadel. Replacing a different existing
	// citadel entry is not covered by it, so that is declined here and left to
	// `citadel local config --write`, which asks.
	written, err := mergeHarnessConfig(h, path, exe, func(string) bool { return false }, now)
	res := localviewer.SetupConnectResult{Path: path}
	switch {
	case errors.Is(err, errAlreadyConfigured):
		res.Outcome = "already"
		res.Message = fmt.Sprintf("%s already has Citadel; nothing changed.", h.title)
	case errors.Is(err, errWriteDeclined):
		res.Outcome = "declined"
		res.Message = fmt.Sprintf("%s already has a different \"citadel\" entry, so nothing was changed. To replace it, run: %s local config %s --write", h.title, quoteExe(exe), h.name)
		return res, nil
	case err != nil:
		return localviewer.SetupConnectResult{}, err
	default:
		res.Outcome = "written"
		res.Path = written
		res.Backup = existed
		res.Message = fmt.Sprintf("Added Citadel to %s.", written)
		if existed {
			res.Message += " The previous file was saved beside it with a .bak name."
		}
	}
	res.NextStep = harnessNextStep(h)
	return res, nil
}

// genericEntry is the copyable MCP entry for apps Citadel has no writer for.
func genericEntry(exe string) string {
	return renderBlock(harnessSpec{
		entry: func(exe string) map[string]any { return map[string]any{"command": exe, "args": localArgs} },
	}, exe)
}

type localSetupDeps struct {
	stateDir func() string
	env      func() harnessEnv
	doctor   func(full bool) []localviewer.SetupCheck
	exe      string
	openURL  func(string) error
	now      func() time.Time
}

func realLocalSetupDeps() (localSetupDeps, error) {
	exe, err := citadelExecutable()
	if err != nil {
		return localSetupDeps{}, fmt.Errorf("find the citadel executable: %w", err)
	}
	return localSetupDeps{
		stateDir: localStateDir,
		env:      systemHarnessEnv,
		exe:      exe,
		openURL:  platform.OpenURL,
		now:      time.Now,
		doctor: func(full bool) []localviewer.SetupCheck {
			checks, _ := collectLocalDoctor(realLocalDoctorDeps(!full))
			out := make([]localviewer.SetupCheck, len(checks))
			for i, c := range checks {
				out[i] = localviewer.SetupCheck(c)
			}
			return out
		},
	}, nil
}

// newCachedSetupDoctor runs the doctor once now (setup start) and again only
// when the full check is requested. The page polls /api/setup/state every few
// seconds; serving the cached result keeps those polls from probing the
// co-browse owner lock (which would refuse an agent's first start with the
// setup PID) or re-verifying the audit chain.
func newCachedSetupDoctor(run func(full bool) []localviewer.SetupCheck) func(full bool) []localviewer.SetupCheck {
	var mu sync.Mutex
	cached := run(false)
	return func(full bool) []localviewer.SetupCheck {
		mu.Lock()
		defer mu.Unlock()
		if full {
			cached = run(true)
		}
		return cached
	}
}

// newLocalSetupServer builds the setup page's server. It uses its own private
// dir so it never replaces a running co-browse owner's token file.
func newLocalSetupServer(d localSetupDeps) *localviewer.Server {
	dir := d.stateDir()
	return localviewer.New(localviewer.Config{
		TokenDir:    dir,
		PrivateDir:  localviewer.SetupDirName,
		OpenURL:     d.openURL,
		Now:         d.now,
		MaxSessions: localcobrowse.DefaultMaxSessions,
		Setup: &localviewer.SetupConfig{
			Doctor:      newCachedSetupDoctor(d.doctor),
			Apps:        func() []localviewer.SetupApp { return setupApps(d.env()) },
			MaxSessions: localcobrowse.DefaultMaxSessions,
			Connect: func(name, path string) (localviewer.SetupConnectResult, error) {
				return setupConnect(name, path, d.exe, d.env(), d.now())
			},
			Sessions: func() ([]localviewer.SetupSession, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				s, _, err := localviewer.RequestSummary(ctx, dir, nil)
				return s, err
			},
			OpenViewer: func() error {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, file, err := localviewer.RequestLaunch(ctx, dir, nil)
				if err != nil {
					return err
				}
				if file == "" {
					return errors.New("the viewer could not write its one-time launch file")
				}
				return d.openURL(file)
			},
			GenericEntry: genericEntry(d.exe),
		},
	})
}

// runLocalSetup serves the setup page, opens it, and blocks until ctx ends
// (a signal, or the console window closing).
func runLocalSetup(ctx context.Context, d localSetupDeps, out io.Writer) error {
	srv := newLocalSetupServer(d)
	if err := srv.Start(); err != nil {
		return err
	}
	defer srv.Close()
	if err := srv.Open(); err != nil {
		link := srv.MintURL()
		fmt.Fprintln(out, "Could not open a browser. Open this link (it works once, for two minutes):")
		fmt.Fprintln(out, link)
	}
	fmt.Fprintln(out, "Setup is open in your browser. Close this window to stop.")
	<-ctx.Done()
	return nil
}

var localSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Open the guided setup page in your browser",
	Long: `Opens a setup page in your default browser: a plain-language check of this
computer, a Connect button for each agent app Citadel finds, and your live
browser sessions. It runs on 127.0.0.1 only, behind a one-time link, and makes
no network calls. On Windows it also opens when you double-click citadel.exe.
Close this window to stop it.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runLocalSetupCommand(cmd.OutOrStdout())
	},
}

func runLocalSetupCommand(out io.Writer) error {
	d, err := realLocalSetupDeps()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runLocalSetup(ctx, d, out)
}

func init() { localCmd.AddCommand(localSetupCmd) }
