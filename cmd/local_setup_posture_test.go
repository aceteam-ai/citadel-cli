package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/localviewer"
)

// Tests for the setup-page posture, the human-only Files and Shell grant,
// Disconnect and the worker start (citadel-cli#1867 C5). Every write goes to
// a t.TempDir() node dir; every side effect beyond it is a fake.

func connectedEnroll(ctx context.Context, o enrollOptions) (*enrollResult, error) {
	o.OnStage(enrollProgress{Stage: enrollStageConnect, NodeName: "desk-pc"})
	return &enrollResult{NodeName: "desk-pc", OrgName: "Example Org"}, nil
}

// TestSetupEnrollWritesTheCoBrowseOnlyPosture: a successful enrollment from
// the setup page writes an explicit permissions file into the node dir it
// enrolled, and the page says what it means in plain words.
func TestSetupEnrollWritesTheCoBrowseOnlyPosture(t *testing.T) {
	f := newSetupFixture(t)
	f.setEnroll(connectedEnroll)
	var seen []string
	f.enrollCall("POST", "/api/setup/enroll/start", &seen)
	st := f.waitEnroll(&seen, stageIs(setupEnrollConnected))
	if st.Posture != setupPostureCopy || !st.WorkerOffer {
		t.Fatalf("connected state must show the posture and offer the worker: %+v", st)
	}
	p, err := config.LoadPermissionsStrict(f.nodeDir)
	if err != nil {
		t.Fatal(err)
	}
	want := config.Permissions{CobrowsePublicOnly: true}
	if *p != want {
		t.Fatalf("posture: got %+v, want every surface off and public-only on", *p)
	}
	raw, err := os.ReadFile(filepath.Join(f.nodeDir, "permissions.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"console: false", "desktop: false", "files: false", "services: false", "ssh: false", "provision: false", "shell: false", "cobrowse_public_only: true"} {
		if !bytes.Contains(raw, []byte(line)) {
			t.Errorf("the permissions file must say %q explicitly:\n%s", line, raw)
		}
	}
	// Other enrollment paths and new nodes keep the historical defaults.
	d := config.DefaultPermissions()
	if !d.Services || !d.SSH || !d.Provision || d.CobrowsePublicOnly {
		t.Fatalf("DefaultPermissions changed: %+v", d)
	}
}

// TestSetupPostureOnlyAfterSuccess: a failed, canceled or "already" result
// writes no permissions file.
func TestSetupPostureOnlyAfterSuccess(t *testing.T) {
	for name, fn := range map[string]func(context.Context, enrollOptions) (*enrollResult, error){
		"already": func(context.Context, enrollOptions) (*enrollResult, error) {
			return nil, &enrollError{Stage: enrollStageCheck, Kind: enrollAlreadyEnrolled, Err: errEnrollAlreadyEnrolled}
		},
		"denied": func(context.Context, enrollOptions) (*enrollResult, error) {
			return nil, &enrollError{Stage: enrollStageAuthorize, Kind: enrollDenied, Err: errors.New("denied")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSetupFixture(t)
			f.setEnroll(fn)
			var seen []string
			f.enrollCall("POST", "/api/setup/enroll/start", &seen)
			st := f.waitEnroll(&seen, stageIs(setupEnrollAlready, setupEnrollFailed))
			if st.Posture != "" || st.WorkerOffer {
				t.Fatalf("%s: %+v", name, st)
			}
			if _, err := os.Stat(filepath.Join(f.nodeDir, "permissions.yaml")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s wrote a permissions file: %v", name, err)
			}
		})
	}
}

// TestSetupPostureKeepsPasscodeAndRefusesADamagedFile: the posture keeps a
// passcode hash, and is never written over a file it cannot parse.
func TestSetupPostureKeepsPasscodeAndRefusesADamagedFile(t *testing.T) {
	dir := t.TempDir()
	p := config.DefaultPermissions()
	p.Files, p.Shell = true, true
	if err := p.SetPasscode("2468"); err != nil {
		t.Fatal(err)
	}
	if err := config.SavePermissions(dir, p); err != nil {
		t.Fatal(err)
	}
	if err := writeSetupPosture(dir); err != nil {
		t.Fatal(err)
	}
	got, err := config.LoadPermissionsStrict(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Files || got.Shell || got.Services || !got.CobrowsePublicOnly || !got.VerifyLegacyPasscode("2468") {
		t.Fatalf("posture over an existing file: %+v", got)
	}

	damaged := t.TempDir()
	bad := []byte("shell: [true\n")
	if err := os.WriteFile(filepath.Join(damaged, "permissions.yaml"), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeSetupPosture(damaged); err == nil {
		t.Fatal("the posture was written over a damaged file")
	}
	if b, _ := os.ReadFile(filepath.Join(damaged, "permissions.yaml")); !bytes.Equal(b, bad) {
		t.Fatal("a damaged file was rewritten")
	}
}

// TestSetupPostureFailureWithholdsTheWorker: when the posture cannot be
// saved, the page says so and the worker start is refused before any step.
func TestSetupPostureFailureWithholdsTheWorker(t *testing.T) {
	f := newSetupFixture(t)
	if err := os.MkdirAll(f.nodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.nodeDir, "permissions.yaml"), []byte("shell: [true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	f.worker.releaseNetwork = func() error { calls = append(calls, "release"); return nil }
	f.worker.enableAutostart = func() error { calls = append(calls, "autostart"); return nil }
	f.worker.start = func(context.Context) error { calls = append(calls, "start"); return nil }
	f.setEnroll(connectedEnroll)
	var seen []string
	f.enrollCall("POST", "/api/setup/enroll/start", &seen)
	st := f.waitEnroll(&seen, stageIs(setupEnrollConnected))
	if st.Posture != setupPostureFailedCopy || st.WorkerOffer {
		t.Fatalf("a failed posture must be shown and withhold the worker: %+v", st)
	}
	code, body := f.enrollCall("POST", "/api/setup/worker/start", nil)
	var refusal struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &refusal); err != nil || code != http.StatusConflict || refusal.Error != workerRefusedCopy {
		t.Fatalf("worker start without a saved posture: %d %s", code, body)
	}
	if len(calls) != 0 {
		t.Fatalf("a refused worker start ran %v", calls)
	}
}

func setupPerms(t *testing.T, f *setupFixture) localviewer.NodePermissionState {
	t.Helper()
	r := f.do("GET", "/api/node-permissions", "", f.good(), true)
	var st localviewer.NodePermissionState
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func setupPermsSet(f *setupFixture, body string) (int, string) {
	r := f.do("POST", "/api/node-permissions", body, f.good(), true)
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}

// TestSetupPageGrantsFilesAndShell: the human turns Files and Shell on and
// off from the page; Shell needs the second confirmation and says it stays
// locked without a passcode. Nothing else in the file changes.
func TestSetupPageGrantsFilesAndShell(t *testing.T) {
	f := newSetupFixture(t)
	// Not connected: nothing to allow, and a change is refused.
	if st := setupPerms(t, f); st.Available {
		t.Fatalf("toggles shown before connecting: %+v", st)
	}
	if code, _ := setupPermsSet(f, `{"confirm":true,"surface":"files","enabled":true}`); code != http.StatusConflict {
		t.Fatalf("a change before connecting: %d", code)
	}
	if _, err := os.Stat(filepath.Join(f.nodeDir, "permissions.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused change wrote the permissions file")
	}

	if err := writeSetupPosture(f.nodeDir); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.connected = true
	f.mu.Unlock()
	st := setupPerms(t, f)
	if !st.Available || st.Files || st.Shell || !st.PublicOnly || st.Workspace != filepath.Join(f.home, "citadel-node", "workspace") {
		t.Fatalf("state after connecting: %+v", st)
	}

	if code, body := setupPermsSet(f, `{"confirm":true,"surface":"files","enabled":true}`); code != http.StatusOK {
		t.Fatalf("files on: %d %s", code, body)
	}
	if code, _ := setupPermsSet(f, `{"confirm":true,"surface":"shell","enabled":true}`); code != http.StatusBadRequest {
		t.Fatalf("shell without the second confirmation: %d", code)
	}
	if p, _ := config.LoadPermissionsStrict(f.nodeDir); p.Shell {
		t.Fatal("shell turned on without the second confirmation")
	}
	code, body := setupPermsSet(f, `{"confirm":true,"surface":"shell","enabled":true,"confirm_shell":true}`)
	if code != http.StatusOK || !strings.Contains(body, "citadel passcode set") {
		t.Fatalf("shell on without a passcode must say it stays locked: %d %s", code, body)
	}
	p, err := config.LoadPermissionsStrict(f.nodeDir)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Files || !p.Shell || p.Console || p.Desktop || p.Services || p.SSH || p.Provision || !p.CobrowsePublicOnly {
		t.Fatalf("a grant changed another setting: %+v", p)
	}
	for _, body := range []string{`{"confirm":true,"surface":"files","enabled":false}`, `{"confirm":true,"surface":"shell","enabled":false}`} {
		if code, b := setupPermsSet(f, body); code != http.StatusOK {
			t.Fatalf("turn off: %d %s", code, b)
		}
	}
	if p, _ := config.LoadPermissionsStrict(f.nodeDir); p.Files || p.Shell {
		t.Fatalf("turning off did not persist: %+v", p)
	}
}

// TestSetupPageGrantRefusedWhileEnrolling: no permission changes while an
// enrollment runs.
func TestSetupPageGrantRefusedWhileEnrolling(t *testing.T) {
	f := newSetupFixture(t)
	if err := writeSetupPosture(f.nodeDir); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.connected = true
	f.mu.Unlock()
	var seen []string
	f.enrollCall("POST", "/api/setup/enroll/start", &seen)
	f.waitEnroll(&seen, stageIs(setupEnrollWaiting))
	if code, body := setupPermsSet(f, `{"confirm":true,"surface":"files","enabled":true}`); code != http.StatusConflict || !strings.Contains(body, nodePermsBusyCopy) {
		t.Fatalf("change during an enrollment: %d %s", code, body)
	}
	if p, _ := config.LoadPermissionsStrict(f.nodeDir); p.Files {
		t.Fatal("files turned on during an enrollment")
	}
	if code, _ := f.enrollCall("POST", "/api/setup/disconnect", nil); code != http.StatusConflict {
		t.Fatalf("disconnect during an enrollment: %d", code)
	}
}

// TestSetNodeSurfaceRefusesADamagedFile: the CLI and the page never rewrite a
// damaged permissions file from the defaults, which would turn services, SSH
// and provisioning back on and drop the public-only guard.
func TestSetNodeSurfaceRefusesADamagedFile(t *testing.T) {
	dir := t.TempDir()
	bad := []byte("cobrowse_public_only: true\nservices: [false\n")
	if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := setNodeSurface(dir, localviewer.SurfaceFiles, true, "cli"); err == nil {
		t.Fatal("files was set over a damaged file")
	}
	if _, err := setShellPermission(dir, true); err == nil {
		t.Fatal("shell was set over a damaged file")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "permissions.yaml")); !bytes.Equal(b, bad) {
		t.Fatal("a damaged file was rewritten")
	}
	if _, err := setNodeSurface(t.TempDir(), "console", true, "cli"); !errors.Is(err, errUnknownSurface) {
		t.Fatalf("console must not be grantable here: %v", err)
	}
}

// TestNodePermissionGrantHasNoMCPPath: no local MCP tool can change a node
// permission, disconnect the PC or start the worker, and only the setup page,
// the viewer wiring and the CLI reference the functions that do.
func TestNodePermissionGrantHasNoMCPPath(t *testing.T) {
	rt := newLocalCobrowseRuntime(localCobrowseOptions{ownerDir: func() string { return t.TempDir() }, browser: newCmdFakeBrowser(), policy: allowAllURLs})
	defer rt.close()
	bad := regexp.MustCompile(`(?i)permission|grant|allow|shell|posture|disconnect|logout|autostart|worker|passcode`)
	for _, tool := range newLocalMCPTools(localMCPDeps{cobrowse: rt, workspaceDir: t.TempDir(), egressRelayConfigDir: t.TempDir}) {
		if bad.MatchString(tool.Name) {
			t.Errorf("local MCP tool %s looks like a permission, disconnect or worker path", tool.Name)
		}
	}
	names := []string{"local_permission_set", "local_files_enable", "local_shell_enable", "local_node_permissions", "local_disconnect", "local_worker_start"}
	var lines []string
	for i, n := range names {
		lines = append(lines, `{"jsonrpc":"2.0","id":`+jsonString2(i+1)+`,"method":"tools/call","params":{"name":"`+n+`","arguments":{"surface":"shell","enabled":true}}}`)
	}
	rsp := runLocalBridge(t, rt, nil, lines...)
	for i, n := range names {
		e, _ := rsp[jsonString2(i+1)]["error"].(map[string]any)
		if e == nil || e["code"] != float64(-32601) {
			t.Errorf("%s must be refused as unavailable: %v", n, rsp[jsonString2(i+1)])
		}
	}

	allowed := map[string][]string{
		"setNodeSurface(":         {"local_setup_posture.go", "permission.go"},
		"newNodePermissionHooks(": {"local_setup.go", "local_setup_posture.go"},
		"viewerNodePermissions(":  {"local.go", "local_setup_posture.go", "mcp.go"},
		"disconnectThisPC(":       {"local_disconnect.go", "local_setup.go"},
		"startSetupWorker(":       {"local_setup.go", "local_setup_worker.go"},
		"writeSetupPosture":       {"local_setup.go", "local_setup_posture.go"},
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for sym := range allowed {
			if bytes.Contains(src, []byte(sym)) {
				got[sym] = append(got[sym], name)
			}
		}
	}
	for sym, want := range allowed {
		sort.Strings(got[sym])
		if strings.Join(got[sym], ",") != strings.Join(want, ",") {
			t.Errorf("%s is referenced from %v, want only %v", sym, got[sym], want)
		}
	}
	// The viewer hook is passed only as the viewer's option, never to a tool.
	for _, name := range []string{"mcp.go", "local.go"} {
		src, _ := os.ReadFile(name)
		for _, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, "viewerNodePermissions(") && !strings.Contains(line, "nodePermissions: viewerNodePermissions()") {
				t.Errorf("%s: viewerNodePermissions used outside the viewer option: %s", name, strings.TrimSpace(line))
			}
		}
	}
}

func recordDisconnect(calls *[]string, stopErr, autoErr, logoutErr error, stopApplicable bool, out logoutOutcome) disconnectDeps {
	return disconnectDeps{
		stopWorker: func() (bool, error) {
			*calls = append(*calls, "stop")
			return stopApplicable, stopErr
		},
		disableAutostart: func() (bool, error) {
			*calls = append(*calls, "autostart")
			return stopApplicable, autoErr
		},
		logout: func() (logoutOutcome, error) {
			*calls = append(*calls, "logout")
			return out, logoutErr
		},
	}
}

func stepStatuses(res localviewer.SetupActionResult) string {
	var s []string
	for _, st := range res.Steps {
		s = append(s, st.Status)
	}
	return strings.Join(s, ",")
}

// TestDisconnectRunsStopAutostartLogoutInOrder covers the full run and every
// partial failure.
func TestDisconnectRunsStopAutostartLogoutInOrder(t *testing.T) {
	boom := errors.New("raw failure text")
	cases := []struct {
		name                   string
		stopErr, autoErr, lErr error
		applicable             bool
		out                    logoutOutcome
		wantCalls, wantStatus  string
		wantOK                 bool
		wantLogoutCopy         string
	}{
		{"all done, deregistered", nil, nil, nil, true, logoutOutcome{Deregistered: true}, "stop,autostart,logout", "ok,ok,ok", true, disconnectLogoutOKCopy},
		{"all done, local logout only", nil, nil, nil, true, logoutOutcome{}, "stop,autostart,logout", "ok,ok,ok", true, disconnectLogoutLocalCopy},
		{"not connected", nil, nil, nil, true, logoutOutcome{NotConnected: true}, "stop,autostart,logout", "ok,ok,ok", true, disconnectLogoutNoneCopy},
		{"not windows", nil, nil, nil, false, logoutOutcome{}, "stop,autostart,logout", "not_applicable,not_applicable,ok", true, disconnectLogoutLocalCopy},
		{"stop fails: logout skipped, autostart still removed", boom, nil, nil, true, logoutOutcome{}, "stop,autostart", "failed,ok,skipped", false, disconnectLogoutSkipCopy},
		{"autostart fails: logout still runs", nil, boom, nil, true, logoutOutcome{}, "stop,autostart,logout", "ok,failed,ok", false, disconnectLogoutLocalCopy},
		{"logout held by another process", nil, nil, errLogoutHeld, true, logoutOutcome{}, "stop,autostart,logout", "ok,ok,failed", false, disconnectLogoutHeldCopy},
		{"logout fails", nil, nil, boom, true, logoutOutcome{}, "stop,autostart,logout", "ok,ok,failed", false, disconnectLogoutFailCopy},
	}
	for _, c := range cases {
		var calls []string
		res := disconnectThisPC(recordDisconnect(&calls, c.stopErr, c.autoErr, c.lErr, c.applicable, c.out))
		if got := strings.Join(calls, ","); got != c.wantCalls {
			t.Errorf("%s: calls %s, want %s", c.name, got, c.wantCalls)
		}
		if got := stepStatuses(res); got != c.wantStatus {
			t.Errorf("%s: statuses %s, want %s", c.name, got, c.wantStatus)
		}
		if res.OK != c.wantOK || len(res.Steps) != 3 || res.Steps[2].Message != c.wantLogoutCopy {
			t.Errorf("%s: %+v", c.name, res)
		}
		if b, _ := json.Marshal(res); bytes.Contains(b, []byte(boom.Error())) {
			t.Errorf("%s: raw error text reached the result", c.name)
		}
	}
}

// TestLogoutThisNode: deregistration is best effort, the local logout always
// follows it, and nothing runs while another process holds the state.
func TestLogoutThisNode(t *testing.T) {
	var calls []string
	deps := func(state bool, precheck, dereg error) logoutDeps {
		calls = nil
		return logoutDeps{
			hasState:    func() bool { return state },
			isConnected: func() bool { return false },
			precheck:    func() error { calls = append(calls, "precheck"); return precheck },
			deregister:  func(context.Context) error { calls = append(calls, "deregister"); return dereg },
			localLogout: func() error { calls = append(calls, "logout"); return nil },
			clearSessionMode: func() error {
				calls = append(calls, "session")
				return nil
			},
		}
	}
	if out, err := logoutThisNode(context.Background(), deps(false, nil, nil)); err != nil || !out.NotConnected || len(calls) != 0 {
		t.Fatalf("not connected: %+v %v %v", out, err, calls)
	}
	if _, err := logoutThisNode(context.Background(), deps(true, errors.New("worker running"), nil)); !errors.Is(err, errLogoutHeld) || strings.Join(calls, ",") != "precheck" {
		t.Fatalf("held: %v %v", err, calls)
	}
	out, err := logoutThisNode(context.Background(), deps(true, nil, errors.New("no API key configured")))
	if err != nil || out.Deregistered || strings.Join(calls, ",") != "precheck,deregister,logout,session" {
		t.Fatalf("deregistration failure must still log out: %+v %v %v", out, err, calls)
	}
	out, err = logoutThisNode(context.Background(), deps(true, nil, nil))
	if err != nil || !out.Deregistered {
		t.Fatalf("deregistered: %+v %v", out, err)
	}
}

// TestLocalDisconnectCommandAsksFirst: the CLI changes nothing without a yes,
// and reports each step.
func TestLocalDisconnectCommandAsksFirst(t *testing.T) {
	var calls []string
	var out bytes.Buffer
	if err := runLocalDisconnect(strings.NewReader("n\n"), &out, false, recordDisconnect(&calls, nil, nil, nil, true, logoutOutcome{})); err != nil || len(calls) != 0 {
		t.Fatalf("declined: %v %v", err, calls)
	}
	out.Reset()
	if err := runLocalDisconnect(strings.NewReader("y\n"), &out, false, recordDisconnect(&calls, nil, nil, nil, true, logoutOutcome{})); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "stop,autostart,logout" || !strings.Contains(out.String(), disconnectStepLogout+": done.") || !strings.Contains(out.String(), disconnectDoneCopy) {
		t.Fatalf("confirmed: %v\n%s", calls, out.String())
	}
	calls = nil
	out.Reset()
	if err := runLocalDisconnect(strings.NewReader(""), &out, true, recordDisconnect(&calls, errors.New("x"), nil, nil, true, logoutOutcome{})); err == nil {
		t.Fatal("a partial disconnect must fail the command")
	}
	if !strings.Contains(out.String(), disconnectStepStop+": failed.") || !strings.Contains(out.String(), disconnectStepLogout+": skipped.") {
		t.Fatalf("partial:\n%s", out.String())
	}
}

// TestSetupPageDisconnect: the page's button needs the confirmation and runs
// the steps in order.
func TestSetupPageDisconnect(t *testing.T) {
	f := newSetupFixture(t)
	var calls []string
	f.mu.Lock()
	f.disconnect = recordDisconnect(&calls, nil, nil, nil, true, logoutOutcome{})
	f.mu.Unlock()
	if r := f.do("POST", "/api/setup/disconnect", `{}`, f.good(), true); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("disconnect without confirmation: %d", r.StatusCode)
	}
	h := f.good()
	h["Origin"] = "http://attacker.test"
	if r := f.do("POST", "/api/setup/disconnect", `{"confirm":true}`, h, true); r.StatusCode != http.StatusForbidden {
		t.Fatalf("disconnect from a foreign origin: %d", r.StatusCode)
	}
	if r := f.do("POST", "/api/setup/disconnect", `{"confirm":true}`, f.good(), false); r.StatusCode != http.StatusForbidden {
		t.Fatalf("disconnect without the cookie: %d", r.StatusCode)
	}
	if len(calls) != 0 {
		t.Fatalf("a refused request ran %v", calls)
	}
	code, body := f.enrollCall("POST", "/api/setup/disconnect", nil)
	var res localviewer.SetupActionResult
	if err := json.Unmarshal([]byte(body), &res); err != nil || code != http.StatusOK || !res.OK || strings.Join(calls, ",") != "stop,autostart,logout" {
		t.Fatalf("disconnect: %d %s %v", code, body, calls)
	}
	if st := f.state(); !st.DisconnectAvailable {
		t.Fatal("the page must offer Disconnect")
	}
}

func recordWorker(calls *[]string, goos string, offered, posture bool, releaseErr, autoErr, startErr error) setupWorkerDeps {
	return setupWorkerDeps{
		goos:            goos,
		offered:         func() bool { return offered },
		postureHolds:    func() bool { return posture },
		releaseNetwork:  func() error { *calls = append(*calls, "release"); return releaseErr },
		enableAutostart: func() error { *calls = append(*calls, "autostart"); return autoErr },
		start:           func(context.Context) error { *calls = append(*calls, "start"); return startErr },
	}
}

// TestStartSetupWorker covers the start-worker flow.
func TestStartSetupWorker(t *testing.T) {
	boom := errors.New("x")
	var calls []string
	res, err := startSetupWorker(recordWorker(&calls, "linux", true, true, nil, nil, nil))
	if err != nil || res.Message != workerOtherOSCopy || len(calls) != 0 {
		t.Fatalf("other OS: %+v %v %v", res, err, calls)
	}
	for _, c := range []struct{ offered, posture bool }{{false, true}, {true, false}} {
		calls = nil
		if _, err := startSetupWorker(recordWorker(&calls, "windows", c.offered, c.posture, nil, nil, nil)); err == nil || len(calls) != 0 {
			t.Fatalf("offered=%v posture=%v: %v %v", c.offered, c.posture, err, calls)
		}
	}
	cases := []struct {
		name                      string
		releaseErr, autoErr, sErr error
		wantCalls, wantStatus     string
		wantOK                    bool
	}{
		{"all done", nil, nil, nil, "release,autostart,start", "ok,ok,ok", true},
		{"release fails: nothing else runs", boom, nil, nil, "release", "failed", false},
		{"autostart fails: still starts now", nil, boom, nil, "release,autostart,start", "ok,failed,ok", false},
		{"start fails", nil, nil, boom, "release,autostart,start", "ok,ok,failed", false},
	}
	for _, c := range cases {
		calls = nil
		res, err := startSetupWorker(recordWorker(&calls, "windows", true, true, c.releaseErr, c.autoErr, c.sErr))
		if err != nil || strings.Join(calls, ",") != c.wantCalls || stepStatuses(res) != c.wantStatus || res.OK != c.wantOK {
			t.Errorf("%s: %+v %v %v", c.name, res, err, calls)
		}
	}
}

// TestSetupPageStartsTheWorkerAfterConnect: the worker start is offered only
// after this page connected the PC, and runs the steps in order.
func TestSetupPageStartsTheWorkerAfterConnect(t *testing.T) {
	f := newSetupFixture(t)
	var calls []string
	f.mu.Lock()
	f.worker.releaseNetwork = func() error { calls = append(calls, "release"); return nil }
	f.worker.enableAutostart = func() error { calls = append(calls, "autostart"); return nil }
	f.worker.start = func(context.Context) error { calls = append(calls, "start"); return nil }
	f.mu.Unlock()
	if code, _ := f.enrollCall("POST", "/api/setup/worker/start", nil); code != http.StatusConflict || len(calls) != 0 {
		t.Fatalf("worker start before connecting: %d %v", code, calls)
	}
	f.setEnroll(connectedEnroll)
	var seen []string
	f.enrollCall("POST", "/api/setup/enroll/start", &seen)
	f.waitEnroll(&seen, stageIs(setupEnrollConnected))
	if r := f.do("POST", "/api/setup/worker/start", `{}`, f.good(), true); r.StatusCode != http.StatusBadRequest || len(calls) != 0 {
		t.Fatalf("worker start without confirmation: %d %v", r.StatusCode, calls)
	}
	code, body := f.enrollCall("POST", "/api/setup/worker/start", nil)
	var res localviewer.SetupActionResult
	if err := json.Unmarshal([]byte(body), &res); err != nil || code != http.StatusOK || !res.OK || strings.Join(calls, ",") != "release,autostart,start" {
		t.Fatalf("worker start: %d %s %v", code, body, calls)
	}
	// The posture must still hold at click time: a later change refuses.
	if err := setCobrowsePublicOnly(f.nodeDir, false); err != nil {
		t.Fatal(err)
	}
	calls = nil
	if code, _ := f.enrollCall("POST", "/api/setup/worker/start", nil); code != http.StatusConflict || len(calls) != 0 {
		t.Fatalf("worker start without the public-only guard: %d %v", code, calls)
	}
}
