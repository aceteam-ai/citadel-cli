package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/localaudit"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/service"
)

// macLaunchctl is a launchctl fake over a job table, recording every call into
// a shared log so a test can order launchd calls against logout and key
// removal.
type macLaunchctl struct {
	log          *[]string
	loaded       bool
	bootoutFails bool
	printExit    int // non-zero: print exits with it (an SSH-style unreachable domain)
}

func (m *macLaunchctl) run(_ context.Context, args ...string) ([]byte, error) {
	*m.log = append(*m.log, "launchctl "+args[0])
	exit := &service.LaunchctlExit{Code: 113}
	switch args[0] {
	case "print":
		if m.printExit != 0 {
			return nil, &service.LaunchctlExit{Code: m.printExit}
		}
		if !m.loaded {
			return nil, exit
		}
		return []byte("\tpid = 99\n"), nil
	case "print-disabled":
		return []byte("disabled services = {\n}\n"), nil
	case "bootout":
		if m.bootoutFails {
			return nil, exit
		}
		m.loaded = false
		return nil, nil
	case "bootstrap":
		m.loaded = true
		return nil, nil
	}
	return nil, errors.New("unexpected " + args[0])
}

const macTestExe = "/Users/test/.local/bin/citadel"

func newMacAgent(t *testing.T, log *[]string) (*service.LaunchAgent, *macLaunchctl) {
	t.Helper()
	m := &macLaunchctl{log: log}
	return &service.LaunchAgent{
		Home: t.TempDir(), UID: 501, Euid: 501, Run: m.run,
		StatExe: func(string) (service.ExeInfo, error) { return service.ExeInfo{UID: 501, Mode: 0o755}, nil },
	}, m
}

// macSetupDeps is the production macOS worker deps with the agent injected:
// the same enable and start steps darwinSetupWorkerDeps wires, over a temp home.
func macSetupDeps(a *service.LaunchAgent, calls *[]string, posture bool) setupWorkerDeps {
	return setupWorkerDeps{
		goos:            "darwin",
		offered:         func() bool { return true },
		registered:      func() bool { return true },
		postureHolds:    func() bool { return posture },
		releaseNetwork:  func() error { *calls = append(*calls, "release"); return nil },
		enableAutostart: func() error { *calls = append(*calls, "autostart"); return a.Enable(macTestExe) },
		start:           func(context.Context) error { *calls = append(*calls, "start"); return a.Start(context.Background()) },
	}
}

func TestSetupWorkerMacInstallsAndStartsTheLaunchAgent(t *testing.T) {
	var calls []string
	a, m := newMacAgent(t, &calls)
	res, err := startSetupWorker(macSetupDeps(a, &calls, true))
	if err != nil || !res.OK {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	// The same three result lines as Windows, in the same order.
	var names []string
	for _, s := range res.Steps {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, "|"); got != workerStepHandOff+"|"+workerStepAutostart+"|"+workerStepStart {
		t.Fatalf("steps = %s", got)
	}
	if stepStatuses(res) != "ok,ok,ok" || res.Message != workerDoneCopy {
		t.Fatalf("res = %+v", res)
	}
	if !m.loaded {
		t.Error("the agent was not loaded")
	}
	if _, err := os.Stat(a.PlistPath()); err != nil {
		t.Errorf("no plist: %v", err)
	}
	var steps []string
	for _, c := range calls {
		if !strings.HasPrefix(c, "launchctl ") {
			steps = append(steps, c)
		}
	}
	if strings.Join(steps, ",") != "release,autostart,start" {
		t.Errorf("order = %v", calls)
	}
	for _, s := range res.Steps {
		if strings.Contains(s.Message, "--background") || strings.Contains(s.Message, "Task Manager") {
			t.Errorf("Windows copy on macOS: %q", s.Message)
		}
	}

	// A second click is a no-op for a healthy worker.
	calls = nil
	*m.log = nil
	res, err = startSetupWorker(macSetupDeps(a, &calls, true))
	if err != nil || !res.OK {
		t.Fatalf("second click: %+v %v", res, err)
	}
	for _, c := range *m.log {
		if c == "launchctl bootout" || c == "launchctl bootstrap" {
			t.Errorf("a running worker was bounced: %v", *m.log)
		}
	}
}

func TestSetupWorkerMacRefusalsAndFailures(t *testing.T) {
	var calls []string
	a, _ := newMacAgent(t, &calls)
	if _, err := startSetupWorker(macSetupDeps(a, &calls, false)); err == nil || !strings.Contains(err.Error(), "co-browse-only") || len(calls) != 0 {
		t.Fatalf("posture refusal: %v %v", err, calls)
	}
	if _, serr := os.Stat(a.PlistPath()); serr == nil {
		t.Error("a plist was written without the posture")
	}

	// Root never gets an agent: the enable step fails and nothing is loaded.
	a.Euid = 0
	calls = nil
	res, err := startSetupWorker(macSetupDeps(a, &calls, true))
	if err != nil || res.OK || stepStatuses(res) != "ok,failed,skipped" {
		t.Fatalf("root: %+v %v", res, err)
	}
	if _, serr := os.Stat(a.PlistPath()); serr == nil {
		t.Error("root wrote a plist")
	}
}

// TestDisconnectMacStopsTheAgentBeforeLogoutAndKeyRemoval pins the order:
// bootout, plist removal, then logout, then the device key.
func TestDisconnectMacStopsTheAgentBeforeLogoutAndKeyRemoval(t *testing.T) {
	var log []string
	a, m := newMacAgent(t, &log)
	if err := a.Enable(macTestExe); err != nil {
		t.Fatal(err)
	}
	m.loaded = true
	log = nil

	base := disconnectDeps{
		logout: func() (logoutOutcome, error) {
			log = append(log, "logout")
			return logoutOutcome{Deregistered: true}, nil
		},
		removeCredentials: func() (bool, error) {
			log = append(log, "remove key")
			if _, err := os.Stat(a.PlistPath()); err == nil {
				t.Error("the plist still existed when the key was removed")
			}
			return true, nil
		},
	}
	res := disconnectThisPC(darwinDisconnectDeps(base, a, nil))
	if !res.OK {
		t.Fatalf("res = %+v", res)
	}
	var verbs []string
	for _, l := range log {
		if l != "launchctl print" && l != "launchctl print-disabled" {
			verbs = append(verbs, l)
		}
	}
	if got := strings.Join(verbs, ","); got != "launchctl bootout,logout,remove key" {
		t.Fatalf("order = %s (full log %v)", got, log)
	}
	if m.loaded {
		t.Error("the job is still loaded")
	}
	if _, err := os.Stat(a.PlistPath()); err == nil {
		t.Error("the plist was not removed")
	}
	if res.Steps[0].Message != disconnectStopOKMacCopy {
		t.Errorf("stop copy = %q", res.Steps[0].Message)
	}
}

func TestDisconnectMacBootoutFailureStopsTheDisconnect(t *testing.T) {
	var log []string
	a, m := newMacAgent(t, &log)
	if err := a.Enable(macTestExe); err != nil {
		t.Fatal(err)
	}
	m.loaded, m.bootoutFails = true, true
	m.printExit = 0
	base := disconnectDeps{
		logout:            func() (logoutOutcome, error) { log = append(log, "logout"); return logoutOutcome{}, nil },
		removeCredentials: func() (bool, error) { log = append(log, "remove key"); return true, nil },
	}
	res := disconnectThisPC(darwinDisconnectDeps(base, a, nil))
	if res.OK || stepStatuses(res) != "failed,failed,skipped,skipped" {
		t.Fatalf("res = %+v", res)
	}
	for _, l := range log {
		if l == "logout" || l == "remove key" {
			t.Fatalf("%s ran after a failed bootout: %v", l, log)
		}
	}
	if res.Steps[0].Message != disconnectStopFailedMacCopy || strings.Contains(res.Steps[0].Message, "Task Manager") {
		t.Errorf("message = %q", res.Steps[0].Message)
	}
	if !strings.Contains(res.Steps[0].Message, "citadel worker stop") {
		t.Error("the message must say what to run")
	}
	if _, err := os.Stat(a.PlistPath()); err != nil {
		t.Errorf("the plist was removed although the stop failed: %v", err)
	}

	// A failure to even build the agent also clears nothing.
	log = nil
	res = disconnectThisPC(darwinDisconnectDeps(base, nil, errors.New("no home")))
	if res.OK || len(log) != 0 {
		t.Fatalf("agent error: %+v %v", res, log)
	}
}

func TestLocalDoctorReportsTheMacLaunchAgent(t *testing.T) {
	base := func(uw userWorkerState) localDoctorDeps {
		return localDoctorDeps{
			discover:   func() (platform.BrowserInfo, error) { return platform.BrowserInfo{Kind: "chrome", Path: "/x"}, nil },
			bindTest:   func() error { return nil },
			vault:      func() (bool, error) { return false, nil },
			owner:      func() error { return nil },
			audit:      func() (localaudit.VerifyResult, error) { return localaudit.VerifyResult{}, nil },
			userWorker: func() userWorkerState { return uw },
			exe:        "citadel",
		}
	}
	find := func(checks []localDoctorCheck, what string) (localDoctorCheck, bool) {
		for _, c := range checks {
			if c.What == what {
				return c, true
			}
		}
		return localDoctorCheck{}, false
	}
	run := func(st service.LaunchAgentState) []localDoctorCheck {
		checks, failed := collectLocalDoctor(base(userWorkerStateFromAgent(st)))
		if failed {
			t.Fatal("an agent state failed the doctor")
		}
		return checks
	}
	plist := "/Users/x/Library/LaunchAgents/ai.aceteam.citadel.plist"

	checks := run(service.LaunchAgentState{PlistPath: plist})
	if c, _ := find(checks, "Autostart"); c.Status != "ok" || !strings.Contains(c.Detail, "off") {
		t.Fatalf("absent: %+v", c)
	}
	if c, _ := find(checks, "Worker"); c.Status != "ok" || !strings.Contains(c.Detail, "no background worker") {
		t.Fatalf("absent worker: %+v", c)
	}

	checks = run(service.LaunchAgentState{PlistPath: plist, PlistPresent: true, Loaded: true, PID: 321, PointsAtThis: true, Exe: macTestExe})
	if c, _ := find(checks, "Autostart"); c.Status != "ok" || !strings.Contains(c.Detail, macTestExe) {
		t.Fatalf("on: %+v", c)
	}
	if c, _ := find(checks, "Worker"); c.Status != "ok" || !strings.Contains(c.Detail, "321") || !strings.Contains(c.Detail, "worker stop") {
		t.Fatalf("running: %+v", c)
	}

	checks = run(service.LaunchAgentState{PlistPath: plist, PlistPresent: true, Disabled: true, PointsAtThis: true})
	if c, _ := find(checks, "Autostart"); c.Status != "note" || !strings.Contains(c.Detail, "Login Items") {
		t.Fatalf("disabled: %+v", c)
	}
	if c, _ := find(checks, "Worker"); c.Status != "ok" {
		t.Fatalf("disabled worker: %+v", c)
	}

	checks = run(service.LaunchAgentState{PlistPath: plist, PlistPresent: true, Loaded: true})
	if c, _ := find(checks, "Worker"); c.Status != "note" || !strings.Contains(c.Detail, "not running") {
		t.Fatalf("loaded, no pid: %+v", c)
	}

	checks = run(service.LaunchAgentState{PlistPath: plist, PlistPresent: true, Exe: "/old/citadel"})
	if c, _ := find(checks, "Autostart"); c.Status != "note" || !strings.Contains(c.Detail, "/old/citadel") {
		t.Fatalf("stale exe: %+v", c)
	}

	checks = run(service.LaunchAgentState{PlistPath: plist, PlistPresent: true, PointsAtThis: true, LogBytes: service.LaunchAgentLogMaxBytes + 1})
	if c, ok := find(checks, "Worker log"); !ok || c.Status != "note" {
		t.Fatalf("oversize log: %+v %v", c, ok)
	}
}

// TestMacWorkerDepsWriteOnlyUnderTheInjectedHome is the hermetic-test guard:
// no test above may reach the real home.
func TestMacWorkerDepsWriteOnlyUnderTheInjectedHome(t *testing.T) {
	var log []string
	a, _ := newMacAgent(t, &log)
	realHome, _ := os.UserHomeDir()
	if realHome != "" && strings.HasPrefix(a.Home, realHome) && !strings.HasPrefix(a.Home, os.TempDir()) {
		t.Fatalf("agent home %s is under the real home", a.Home)
	}
	if err := a.Enable(macTestExe); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.PlistPath(), filepath.Clean(a.Home)) {
		t.Fatalf("plist %s is outside %s", a.PlistPath(), a.Home)
	}
}

// TestDisconnectMacSSHStyleFailureKeepsTheCredentials: where gui/<uid> cannot
// be read, Stop errors, so the logout and key removal never run.
func TestDisconnectMacSSHStyleFailureKeepsTheCredentials(t *testing.T) {
	var log []string
	a, m := newMacAgent(t, &log)
	if err := a.Enable(macTestExe); err != nil {
		t.Fatal(err)
	}
	m.printExit = 112
	base := disconnectDeps{
		logout:            func() (logoutOutcome, error) { log = append(log, "logout"); return logoutOutcome{}, nil },
		removeCredentials: func() (bool, error) { log = append(log, "remove key"); return true, nil },
	}
	res := disconnectThisPC(darwinDisconnectDeps(base, a, nil))
	if res.OK || res.Steps[0].Status != stepFailed {
		t.Fatalf("res = %+v", res)
	}
	for _, l := range log {
		if l == "logout" || l == "remove key" {
			t.Fatalf("%s ran: %v", l, log)
		}
	}
	if _, err := os.Stat(a.PlistPath()); err != nil {
		t.Errorf("plist removed: %v", err)
	}
}
