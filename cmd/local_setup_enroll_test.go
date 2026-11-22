package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/localviewer"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// Tests for "Connect this PC to AceTeam" on the setup page (citadel-cli#1867
// C2). No test here reaches the real network or this machine's node config
// dir: the enrollment is either a fake or enrollDevice over newEnrollHarness
// (the device-auth mock, t.TempDir() config, stubbed connect and link).

const setupEnrollSentinel = "SENTINEL-raw-error-text-7f3c"

// waitingFakeEnroll reports a code on the fixture's auth host and waits to be
// canceled, like a real enrollment waiting for approval.
func waitingFakeEnroll(ctx context.Context, o enrollOptions) (*enrollResult, error) {
	o.OnStage(enrollProgress{Stage: enrollStageCheck})
	o.OnStage(enrollProgress{Stage: enrollStageAuthorize})
	o.OnCode(enrollCode{UserCode: "ABCD-1234", VerificationURI: o.AuthServiceURL + "/device", VerificationURIComplete: o.AuthServiceURL + "/device?code=ABCD-1234"})
	<-ctx.Done()
	return nil, &enrollError{Stage: enrollStageAuthorize, Kind: enrollCanceled, Err: ctx.Err()}
}

func (f *setupFixture) setEnroll(fn func(context.Context, enrollOptions) (*enrollResult, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enrollFn = fn
}

func (f *setupFixture) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs
}

func (f *setupFixture) openedURLs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.opened...)
}

// enrollCall makes one request and returns status and body; every body is also
// appended to seen, for the secret checks.
func (f *setupFixture) enrollCall(method, path string, seen *[]string) (int, string) {
	f.t.Helper()
	body := ""
	if method == http.MethodPost {
		body = `{"confirm":true}`
	}
	r := f.do(method, path, body, f.good(), true)
	b, _ := io.ReadAll(r.Body)
	if seen != nil {
		*seen = append(*seen, string(b))
	}
	return r.StatusCode, string(b)
}

func (f *setupFixture) enrollState(seen *[]string) localviewer.SetupEnrollState {
	f.t.Helper()
	code, body := f.enrollCall(http.MethodGet, "/api/setup/enroll/state", seen)
	if code != http.StatusOK {
		f.t.Fatalf("state: %d %s", code, body)
	}
	var st localviewer.SetupEnrollState
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		f.t.Fatal(err)
	}
	return st
}

// waitEnroll polls the state route until want(state) or the deadline.
func (f *setupFixture) waitEnroll(seen *[]string, want func(localviewer.SetupEnrollState) bool) localviewer.SetupEnrollState {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := f.enrollState(seen)
		if want(st) {
			return st
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("enrollment state never matched; last %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func stageIs(stages ...string) func(localviewer.SetupEnrollState) bool {
	return func(st localviewer.SetupEnrollState) bool {
		for _, s := range stages {
			if st.Stage == s {
				return true
			}
		}
		return false
	}
}

// harnessEnroll runs the real enrollDevice over h's mock and stubs, with the
// callbacks the setup page supplies.
func harnessEnroll(h *enrollHarness) func(context.Context, enrollOptions) (*enrollResult, error) {
	return func(ctx context.Context, o enrollOptions) (*enrollResult, error) {
		deps := h.deps
		deps.authorize = func(ctx context.Context) (*DeviceAuthResult, error) {
			return authorizeDeviceNonInteractive(ctx, nexus.NewDeviceAuthClient(h.mock.URL()), o.ForceNew, o.OnCode)
		}
		return enrollDevice(ctx, o, deps)
	}
}

func useHarness(f *setupFixture, h *enrollHarness) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enrollFn = harnessEnroll(h)
	f.optsFn = func() enrollOptions {
		return enrollOptions{AuthServiceURL: h.mock.URL(), ConfigDir: h.configDir}
	}
}

func TestSetupEnrollRoutesAreGuarded(t *testing.T) {
	f := newSetupFixture(t)
	confirm := `{"confirm":true}`
	with := func(k, v string) map[string]string {
		h := f.good()
		if v == "" {
			delete(h, k)
		} else {
			h[k] = v
		}
		return h
	}
	type route struct{ method, path, body string }
	routes := []route{
		{"POST", "/api/setup/enroll/start", confirm},
		{"GET", "/api/setup/enroll/state", ""},
		{"POST", "/api/setup/enroll/cancel", confirm},
		{"GET", "/api/setup/enroll/qr", ""},
	}
	for _, rt := range routes {
		cases := []struct {
			name   string
			hdr    map[string]string
			cookie bool
		}{
			{"foreign origin", with("Origin", "http://attacker.test"), true},
			{"missing bearer", with("X-Citadel-CSRF", ""), true},
			{"wrong bearer", with("X-Citadel-CSRF", "nope"), true},
			{"no cookie", f.good(), false},
			{"cross-site", with("Sec-Fetch-Site", "cross-site"), true},
		}
		if rt.method == "POST" {
			cases = append(cases, struct {
				name   string
				hdr    map[string]string
				cookie bool
			}{"missing origin", with("Origin", ""), true})
		}
		for _, c := range cases {
			if r := f.do(rt.method, rt.path, rt.body, c.hdr, c.cookie); r.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s %s: want 403, got %d", rt.method, rt.path, c.name, r.StatusCode)
			}
		}
	}
	if n := f.runCount(); n != 0 {
		t.Fatalf("a refused request started %d enrollment(s)", n)
	}
	// A start must confirm, like the Connect click.
	if r := f.do("POST", "/api/setup/enroll/start", `{}`, f.good(), true); r.StatusCode != http.StatusBadRequest {
		t.Errorf("start without confirm: %d", r.StatusCode)
	}
	if r := f.do("POST", "/api/setup/enroll/start", confirm, with("Content-Type", "text/plain"), true); r.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("start as a form: %d", r.StatusCode)
	}
	if n := f.runCount(); n != 0 {
		t.Fatalf("an unconfirmed start ran %d enrollment(s)", n)
	}

	// Positive controls: the same routes with the right cookie, bearer and
	// origin are served, so the 403s above came from the guard, not a 404.
	if code, body := f.enrollCall("GET", "/api/setup/enroll/state", nil); code != 200 || !strings.Contains(body, `"stage":"idle"`) {
		t.Fatalf("state: %d %s", code, body)
	}
	if code, _ := f.enrollCall("GET", "/api/setup/enroll/qr", nil); code != http.StatusNotFound {
		t.Fatalf("qr with nothing waiting: %d", code)
	}
	if code, body := f.enrollCall("POST", "/api/setup/enroll/start", nil); code != 200 {
		t.Fatalf("start: %d %s", code, body)
	}
	f.waitEnroll(nil, stageIs(setupEnrollWaiting))
	qr := f.do("GET", "/api/setup/enroll/qr", "", f.good(), true)
	png, _ := io.ReadAll(qr.Body)
	if qr.StatusCode != 200 || qr.Header.Get("Content-Type") != "image/png" || !strings.HasPrefix(string(png), "\x89PNG") {
		t.Fatalf("qr: %d %s", qr.StatusCode, qr.Header.Get("Content-Type"))
	}
	if code, _ := f.enrollCall("POST", "/api/setup/enroll/cancel", nil); code != 200 {
		t.Fatalf("cancel: %d", code)
	}
	f.waitEnroll(nil, stageIs(setupEnrollCanceled))

	// The section is offered only when the page can enroll.
	var st struct {
		EnrollAvailable bool `json:"enroll_available"`
	}
	r := f.do("GET", "/api/setup/state", "", f.good(), true)
	_ = json.NewDecoder(r.Body).Decode(&st)
	if !st.EnrollAvailable {
		t.Fatal("setup state does not offer Connect to AceTeam")
	}
}

func TestSetupEnrollUnavailableWithoutEnrollSeam(t *testing.T) {
	srv, enroller := newLocalSetupServer(localSetupDeps{
		stateDir: t.TempDir, env: func() harnessEnv { return harnessEnv{goos: "linux", home: t.TempDir()} },
		exe: "citadel", now: time.Now, openURL: func(string) error { return nil },
		doctor: func(bool) []localviewer.SetupCheck { return nil },
	})
	if enroller != nil || srv == nil {
		t.Fatal("no enroll seam must mean no enroller")
	}
}

// TestSetupEnrollEndToEndCarriesNoSecrets runs the real enrollDevice through
// the page: the device code, authkey and device API token are real values the
// mock hands out, and no response may carry them.
func TestSetupEnrollEndToEndCarriesNoSecrets(t *testing.T) {
	h := newEnrollHarness(t, 2)
	f := newSetupFixture(t)
	useHarness(f, h)
	var seen []string

	if code, body := f.enrollCall("POST", "/api/setup/enroll/start", &seen); code != 200 {
		t.Fatalf("start: %d %s", code, body)
	}
	st := f.waitEnroll(&seen, stageIs(setupEnrollWaiting, setupEnrollConnecting, setupEnrollConnected))
	if st.Stage == setupEnrollWaiting {
		if st.UserCode == "" || !strings.HasPrefix(st.VerificationURL, h.mock.URL()+"/device?code=") {
			t.Fatalf("waiting state: %+v", st)
		}
		// One enrollment at a time: a second start leaves this one alone.
		code, body := f.enrollCall("POST", "/api/setup/enroll/start", &seen)
		if code != http.StatusConflict || !strings.Contains(body, setupEnrollCopy[enrollBusy]) {
			t.Fatalf("second start: %d %s", code, body)
		}
		f.enrollCall("GET", "/api/setup/enroll/qr", &seen)
	}
	st = f.waitEnroll(&seen, stageIs(setupEnrollConnected, setupEnrollFailed))
	if st.Stage != setupEnrollConnected || st.OrgName != "Test Org" || st.NodeName != "enroll-test-node" || st.Message != setupEnrollConnectedCopy {
		t.Fatalf("success state: %+v", st)
	}
	if st.UserCode != "" || st.VerificationURL != "" {
		t.Fatalf("the code is still shown after connecting: %+v", st)
	}
	if n := f.runCount(); n != 1 {
		t.Fatalf("ran %d enrollments", n)
	}
	// The approval page was opened in the default browser (after the setup
	// page itself), on the mock's own host.
	opened := f.openedURLs()
	if len(opened) != 2 || !strings.HasPrefix(opened[1], h.mock.URL()+"/device?code=") {
		t.Fatalf("opened: %v", opened)
	}
	// The setup state poll (the page's other request) must not leak either.
	r := f.do("GET", "/api/setup/state", "", f.good(), true)
	b, _ := io.ReadAll(r.Body)
	seen = append(seen, string(b), strings.Join(opened, "\n"))
	for _, body := range seen {
		for _, secret := range []string{enrollTestDeviceCode, enrollTestAuthkey, enrollTestAPIToken} {
			if strings.Contains(body, secret) {
				t.Fatalf("a response carries a grant secret %q: %s", secret, body)
			}
		}
	}
}

func TestSetupEnrollSingleConcurrentEnrollment(t *testing.T) {
	f := newSetupFixture(t)
	if code, _ := f.enrollCall("POST", "/api/setup/enroll/start", nil); code != 200 {
		t.Fatalf("start: %d", code)
	}
	first := f.waitEnroll(nil, stageIs(setupEnrollWaiting))
	for i := 0; i < 3; i++ {
		code, body := f.enrollCall("POST", "/api/setup/enroll/start", nil)
		if code != http.StatusConflict || !strings.Contains(body, setupEnrollCopy[enrollBusy]) {
			t.Fatalf("start %d while running: %d %s", i, code, body)
		}
	}
	if st := f.enrollState(nil); st != first {
		t.Fatalf("a refused start changed the running enrollment: %+v -> %+v", first, st)
	}
	if n := f.runCount(); n != 1 {
		t.Fatalf("ran %d enrollments", n)
	}
}

// TestSetupEnrollCoreBusyMapsToBusyCopy covers the core's own single-flight
// refusal, which another caller in this process could trigger.
func TestSetupEnrollCoreBusyMapsToBusyCopy(t *testing.T) {
	h := newEnrollHarness(t, 2)
	f := newSetupFixture(t)
	useHarness(f, h)
	enrollFlight.Lock()
	defer enrollFlight.Unlock()
	if code, _ := f.enrollCall("POST", "/api/setup/enroll/start", nil); code != 200 {
		t.Fatalf("start: %d", code)
	}
	st := f.waitEnroll(nil, stageIs(setupEnrollFailed))
	if st.Failure != string(enrollBusy) || st.Message != setupEnrollCopy[enrollBusy] {
		t.Fatalf("core busy: %+v", st)
	}
}

func TestSetupEnrollCancelReleasesThePoll(t *testing.T) {
	h := newEnrollHarness(t, 1000)
	f := newSetupFixture(t)
	useHarness(f, h)
	if code, _ := f.enrollCall("POST", "/api/setup/enroll/start", nil); code != 200 {
		t.Fatalf("start: %d", code)
	}
	f.waitEnroll(nil, stageIs(setupEnrollWaiting))
	began := time.Now()
	code, body := f.enrollCall("POST", "/api/setup/enroll/cancel", nil)
	if code != 200 || !(strings.Contains(body, `"stage":"canceling"`) || strings.Contains(body, `"stage":"canceled"`)) {
		t.Fatalf("cancel: %d %s", code, body)
	}
	st := f.waitEnroll(nil, stageIs(setupEnrollCanceled, setupEnrollFailed))
	if st.Stage != setupEnrollCanceled || st.Message != setupEnrollCopy[enrollCanceled] {
		t.Fatalf("after cancel: %+v", st)
	}
	if d := time.Since(began); d > 3*time.Second {
		t.Fatalf("cancel took %v to release the poll", d)
	}
	// The core's single-flight slot is free again.
	if !enrollFlight.TryLock() {
		t.Fatal("the canceled enrollment still holds the core")
	}
	enrollFlight.Unlock()
	polls := h.mock.GetPollCount()
	time.Sleep(1500 * time.Millisecond)
	if after := h.mock.GetPollCount(); after != polls {
		t.Fatalf("the poll kept running after cancel: %d -> %d", polls, after)
	}
	// Nothing was saved.
	if cfg := h.config(); cfg["device_api_token"] != nil {
		t.Fatalf("a canceled enrollment saved credentials: %v", cfg)
	}
	// A new start is accepted.
	if code, _ := f.enrollCall("POST", "/api/setup/enroll/start", nil); code != 200 {
		t.Fatalf("restart: %d", code)
	}
	f.waitEnroll(nil, stageIs(setupEnrollWaiting))
	f.enrollCall("POST", "/api/setup/enroll/cancel", nil)
	f.waitEnroll(nil, stageIs(setupEnrollCanceled))
}

// enrollFailureConstants lists every enrollFailure constant declared in
// enroll_core.go, so a new kind without page copy fails the copy test.
func enrollFailureConstants(t *testing.T) []enrollFailure {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "enroll_core.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []enrollFailure
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			id, ok := vs.Type.(*ast.Ident)
			if !ok || id.Name != "enrollFailure" {
				continue
			}
			for _, v := range vs.Values {
				if lit, ok := v.(*ast.BasicLit); ok {
					out = append(out, enrollFailure(strings.Trim(lit.Value, `"`)))
				}
			}
		}
	}
	if len(out) < 10 {
		t.Fatalf("found only %d enrollFailure constants", len(out))
	}
	return out
}

func TestSetupEnrollCopyCoversEveryFailureKind(t *testing.T) {
	kinds := enrollFailureConstants(t)
	for _, k := range kinds {
		if setupEnrollCopy[k] == "" {
			t.Errorf("no setup page copy for enroll failure %q", k)
		}
	}
	f := newSetupFixture(t)
	f.mu.Lock()
	f.already = func(string) (string, string) { return "desk-pc", "Example Org" }
	f.mu.Unlock()
	for _, k := range kinds {
		kind := k
		f.setEnroll(func(ctx context.Context, o enrollOptions) (*enrollResult, error) {
			o.OnWarn(setupEnrollSentinel)
			return nil, &enrollError{Stage: enrollStageConnect, Kind: kind, Err: errors.New("raw: " + setupEnrollSentinel)}
		})
		var seen []string
		if code, body := f.enrollCall("POST", "/api/setup/enroll/start", &seen); code != 200 {
			t.Fatalf("%s: start %d %s", kind, code, body)
		}
		st := f.waitEnroll(&seen, stageIs(setupEnrollFailed, setupEnrollCanceled, setupEnrollAlready))
		want := setupEnrollFailed
		switch kind {
		case enrollCanceled:
			want = setupEnrollCanceled
		case enrollAlreadyEnrolled, enrollAlreadyConnected:
			want = setupEnrollAlready
			if st.NodeName != "desk-pc" || st.OrgName != "Example Org" {
				t.Errorf("%s: already state does not name the node: %+v", kind, st)
			}
		}
		if st.Stage != want || st.Failure != string(kind) || st.Message != setupEnrollCopy[kind] {
			t.Errorf("%s: state %+v", kind, st)
		}
		for _, b := range seen {
			if strings.Contains(b, setupEnrollSentinel) {
				t.Fatalf("%s: raw error text reached the page: %s", kind, b)
			}
		}
	}
	// An error that is not an enrollError gets the generic copy.
	f.setEnroll(func(context.Context, enrollOptions) (*enrollResult, error) {
		return nil, errors.New(setupEnrollSentinel)
	})
	var seen []string
	f.enrollCall("POST", "/api/setup/enroll/start", &seen)
	st := f.waitEnroll(&seen, stageIs(setupEnrollFailed))
	if st.Message != setupEnrollDefaultCopy || strings.Contains(strings.Join(seen, ""), setupEnrollSentinel) {
		t.Fatalf("untyped error: %+v", st)
	}
}

func TestSetupEnrollSuccessState(t *testing.T) {
	f := newSetupFixture(t)
	f.setEnroll(func(ctx context.Context, o enrollOptions) (*enrollResult, error) {
		o.OnStage(enrollProgress{Stage: enrollStageConnect, NodeName: "desk-pc"})
		return &enrollResult{NodeName: "desk-pc", OrgName: "Example Org", UserEmail: "user@example.test"}, nil
	})
	var seen []string
	f.enrollCall("POST", "/api/setup/enroll/start", &seen)
	st := f.waitEnroll(&seen, stageIs(setupEnrollConnected))
	if st.NodeName != "desk-pc" || st.OrgName != "Example Org" || st.Message != setupEnrollConnectedCopy || st.Failure != "" {
		t.Fatalf("success: %+v", st)
	}
	// An unconfirmed link is a warning: still connected, with fixed copy only.
	f.setEnroll(func(ctx context.Context, o enrollOptions) (*enrollResult, error) {
		o.OnWarn("server said: " + setupEnrollSentinel)
		return &enrollResult{NodeName: "desk-pc", OrgName: "Example Org"}, nil
	})
	f.enrollCall("POST", "/api/setup/enroll/start", &seen)
	st = f.waitEnroll(&seen, func(s localviewer.SetupEnrollState) bool {
		return s.Stage == setupEnrollConnected && s.Message == setupEnrollUnverifiedCopy
	})
	if strings.Contains(strings.Join(seen, ""), setupEnrollSentinel) || strings.Contains(strings.Join(seen, ""), "user@example.test") {
		t.Fatal("the success state carries warning text or the account email")
	}
}

// TestSetupEnrollRefusesAForeignApprovalLink: a verification URL that does
// not point at the enrollment's own AceTeam host is neither opened nor shown,
// and the enrollment is stopped.
func TestSetupEnrollRefusesAForeignApprovalLink(t *testing.T) {
	f := newSetupFixture(t)
	f.setEnroll(func(ctx context.Context, o enrollOptions) (*enrollResult, error) {
		o.OnCode(enrollCode{UserCode: "ABCD-1234", VerificationURIComplete: "file:///C:/Windows/System32/calc.exe"})
		<-ctx.Done()
		return nil, &enrollError{Stage: enrollStageAuthorize, Kind: enrollCanceled, Err: ctx.Err()}
	})
	var seen []string
	f.enrollCall("POST", "/api/setup/enroll/start", &seen)
	st := f.waitEnroll(&seen, stageIs(setupEnrollFailed, setupEnrollCanceled))
	if st.Failure != string(setupEnrollUntrustedLink) || st.Message != setupEnrollCopy[setupEnrollUntrustedLink] {
		t.Fatalf("foreign link: %+v", st)
	}
	if strings.Contains(strings.Join(seen, ""), "calc.exe") {
		t.Fatal("the foreign link was shown")
	}
	if len(f.openedURLs()) != 1 {
		t.Fatalf("the foreign link was opened: %v", f.openedURLs())
	}
}

func TestSetupVerificationURL(t *testing.T) {
	auth := "https://aceteam.example"
	for _, c := range []struct {
		raw, auth, want string
		ok              bool
	}{
		{"https://aceteam.example/device?code=ABCD-1234", auth, "https://aceteam.example/device?code=ABCD-1234", true},
		{"https://aceteam.example/device?code=AB%26x=1&next=//evil", auth, "https://aceteam.example/device?code=AB%26x%3D1", true},
		{"https://other.example/device?code=A", auth, "", false},
		{"http://aceteam.example/device?code=A", auth, "", false},
		{"file:///C:/x.exe", auth, "", false},
		{"javascript:alert(1)", auth, "", false},
		{"https://u:p@aceteam.example/device", auth, "", false},
		{"http://127.0.0.1:9/device?code=A", "http://127.0.0.1:9", "http://127.0.0.1:9/device?code=A", true},
		{"http://10.0.0.1:9/device?code=A", "http://10.0.0.1:9", "", false},
	} {
		got, ok := setupVerificationURL(c.raw, c.auth)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got %q %v, want %q %v", c.raw, got, ok, c.want, c.ok)
		}
		if ok {
			if u, err := url.Parse(got); err != nil || u.Scheme == "" {
				t.Errorf("%s: rebuilt URL does not parse: %q", c.raw, got)
			}
		}
	}
	for _, c := range []string{"", "AB CD", "AB<script>", strings.Repeat("A", 33)} {
		if plausibleUserCode(c) {
			t.Errorf("user code %q accepted", c)
		}
	}
	if !plausibleUserCode("ABCD-1234") {
		t.Error("a normal user code was refused")
	}
}

// TestSetupEnrollShutdownCancelsAndWaits: closing the setup page cancels a
// running enrollment and waits for it to return (and so roll back).
func TestSetupEnrollShutdownCancelsAndWaits(t *testing.T) {
	returned := make(chan struct{})
	e := newSetupEnroller(
		func() enrollOptions {
			return enrollOptions{AuthServiceURL: "https://auth.example.test", ConfigDir: t.TempDir()}
		},
		func(ctx context.Context, o enrollOptions) (*enrollResult, error) {
			<-ctx.Done()
			time.Sleep(50 * time.Millisecond) // the rollback
			close(returned)
			return nil, &enrollError{Kind: enrollCanceled, Err: ctx.Err()}
		},
		nil, nil)
	if _, err := e.Start(); err != nil {
		t.Fatal(err)
	}
	e.Shutdown(5 * time.Second)
	select {
	case <-returned:
	default:
		t.Fatal("Shutdown returned before the enrollment did")
	}
	if st := e.State(); st.Stage != setupEnrollCanceled {
		t.Fatalf("after shutdown: %+v", st)
	}
}
