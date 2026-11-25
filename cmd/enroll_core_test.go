package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"gopkg.in/yaml.v3"
)

// The mock's fixed device code, and grant secrets the enrollment must never
// hand back to its caller.
const (
	enrollTestDeviceCode = "mock-device-code-12345"
	enrollTestAuthkey    = "tskey-auth-enroll-core-test"
	enrollTestAPIToken   = "device-api-token-enroll-core-test"
)

// enrollHarness runs enrollDevice against the device-auth mock with every
// other side effect captured in memory or under t.TempDir(). Nothing here
// resolves the machine's own config dir or touches the network.
type enrollHarness struct {
	t         *testing.T
	mock      *nexus.MockDeviceAuthServer
	configDir string
	opts      enrollOptions
	deps      enrollDeps
	linker    deviceLinker

	mu          sync.Mutex
	codes       []enrollCode
	stages      []enrollProgress
	warnings    []string
	connects    []string
	reclaims    []string
	connectReqs []enrollConnectRequest
	links       int
	disconnects int
}

func newEnrollHarness(t *testing.T, pollsUntilSuccess int) *enrollHarness {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	mock := nexus.StartMockDeviceAuthServer(pollsUntilSuccess)
	t.Cleanup(mock.Close)
	mock.SetSuccessToken(&nexus.TokenResponse{
		Authkey:        enrollTestAuthkey,
		DeviceAPIToken: enrollTestAPIToken,
		APIBaseURL:     "https://api.example.test",
		OrgID:          "org-test",
		OrgName:        "Test Org",
		UserEmail:      "user@example.test",
	})

	h := &enrollHarness{t: t, mock: mock, configDir: t.TempDir(), linker: &successfulLinker{}}
	h.opts = enrollOptions{
		AuthServiceURL: mock.URL(),
		ConfigDir:      h.configDir,
		OnCode: func(c enrollCode) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.codes = append(h.codes, c)
		},
		OnStage: func(p enrollProgress) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.stages = append(h.stages, p)
		},
		OnWarn: func(msg string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.warnings = append(h.warnings, msg)
		},
	}
	h.deps = enrollDeps{
		isConnected:    func() bool { return false },
		checkReachable: nexus.CheckAPIReachable,
		authorize: func(ctx context.Context) (*DeviceAuthResult, error) {
			return authorizeDeviceNonInteractive(ctx, nexus.NewDeviceAuthClient(mock.URL()), h.opts.ForceNew, h.opts.OnCode)
		},
		saveDeviceConfig: saveDeviceConfigToDir,
		saveRedisURL:     saveRedisURLToDir,
		nodeName: func(explicit string) (string, error) {
			if explicit != "" {
				return explicit, nil
			}
			return "enroll-test-node", nil
		},
		reclaim: func(_, hostname string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.reclaims = append(h.reclaims, hostname)
		},
		hasState:          func() bool { return false },
		replacePrecheck:   func() error { return nil },
		resolveControlURL: func(bool, string) string { return "https://control.example.test" },
		connect: func(r enrollConnectRequest) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.connects = append(h.connects, r.NodeName)
			h.connectReqs = append(h.connectReqs, r)
			return nil
		},
		link: func(result *DeviceAuthResult, rollback func(*nexus.TokenResponse) error, warn func(string)) error {
			h.mu.Lock()
			h.links++
			h.mu.Unlock()
			return linkDeviceEnrollmentGatedWith(result, func(context.Context) string { return "42" }, h.linker, rollback, warn)
		},
		disconnect: func() error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.disconnects++
			return nil
		},
	}
	return h
}

func (h *enrollHarness) run(ctx context.Context) (*enrollResult, error) {
	return enrollDevice(ctx, h.opts, h.deps)
}

func (h *enrollHarness) config() map[string]interface{} {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.configDir, "config.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		h.t.Fatalf("read config: %v", err)
	}
	var config map[string]interface{}
	if err := yaml.Unmarshal(data, &config); err != nil {
		h.t.Fatalf("parse config: %v", err)
	}
	return config
}

// assertNoGrantSecrets checks everything the caller of enrollDevice can see.
func (h *enrollHarness) assertNoGrantSecrets(result *enrollResult, err error) {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var seen []string
	if result != nil {
		raw, _ := json.Marshal(result)
		seen = append(seen, string(raw), fmt.Sprintf("%+v", *result), fmt.Sprintf("%#v", *result))
	}
	if err != nil {
		seen = append(seen, err.Error(), fmt.Sprintf("%+v", err))
	}
	for _, c := range h.codes {
		seen = append(seen, fmt.Sprintf("%+v", c))
	}
	for _, p := range h.stages {
		seen = append(seen, fmt.Sprintf("%+v", p))
	}
	seen = append(seen, h.warnings...)
	for _, s := range seen {
		for _, secret := range []string{enrollTestDeviceCode, enrollTestAuthkey, enrollTestAPIToken} {
			if strings.Contains(s, secret) {
				h.t.Fatalf("grant secret %q reached the caller: %s", secret, s)
			}
		}
	}
}

func enrollKind(t *testing.T, err error) enrollFailure {
	t.Helper()
	kind, ok := enrollFailureKind(err)
	if !ok {
		t.Fatalf("want *enrollError, got %T: %v", err, err)
	}
	return kind
}

func TestEnrollDevicePendingThenApproved(t *testing.T) {
	h := newEnrollHarness(t, 2)

	result, err := h.run(context.Background())
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	h.assertNoGrantSecrets(result, err)

	want := enrollResult{NodeName: "enroll-test-node", OrgID: "org-test", OrgName: "Test Org", UserEmail: "user@example.test", APIMode: true}
	if *result != want {
		t.Fatalf("result = %+v, want %+v", *result, want)
	}
	if h.mock.GetPollCount() != 2 {
		t.Fatalf("want approval on the second poll, got %d polls", h.mock.GetPollCount())
	}
	if len(h.codes) != 1 || h.codes[0].UserCode != "MOCK-1234" ||
		h.codes[0].VerificationURIComplete != h.mock.URL()+"/device?code=MOCK-1234" {
		t.Fatalf("OnCode = %+v", h.codes)
	}
	var stages []enrollStage
	for _, p := range h.stages {
		stages = append(stages, p.Stage)
	}
	if fmt.Sprint(stages) != fmt.Sprint([]enrollStage{enrollStageCheck, enrollStageAuthorize, enrollStageSave, enrollStageConnect, enrollStageLink, enrollStageDone}) {
		t.Fatalf("stages = %v", stages)
	}
	if fmt.Sprint(h.connects) != "[enroll-test-node]" || fmt.Sprint(h.reclaims) != "[enroll-test-node]" || h.disconnects != 0 {
		t.Fatalf("connects=%v reclaims=%v disconnects=%d", h.connects, h.reclaims, h.disconnects)
	}
	// The control URL is resolved per enrollment and handed to connect, and
	// the CA trust uses this enrollment's auth service.
	if r := h.connectReqs[0]; r.ControlURL != "https://control.example.test" || r.AuthURL != h.mock.URL() || r.AuthKey != enrollTestAuthkey || r.ForceLogin {
		t.Fatalf("connect request = %+v", r)
	}
	config := h.config()
	if config["device_api_token"] != enrollTestAPIToken || config["org_id"] != "org-test" {
		t.Fatalf("grant not saved to the given config dir: %v", config)
	}
}

func TestEnrollDeviceTerminalRefusals(t *testing.T) {
	for _, tc := range []struct {
		code string
		want enrollFailure
	}{
		{"access_denied", enrollDenied},
		{"expired_token", enrollExpired},
	} {
		t.Run(tc.code, func(t *testing.T) {
			h := newEnrollHarness(t, 1)
			h.mock.SetTerminalTokenError(tc.code)

			result, err := h.run(context.Background())
			if result != nil || enrollKind(t, err) != tc.want {
				t.Fatalf("want %s, got result=%+v err=%v", tc.want, result, err)
			}
			h.assertNoGrantSecrets(result, err)
			if h.config() != nil || len(h.connects) != 0 {
				t.Fatalf("a refused grant must save and connect nothing: config=%v connects=%v", h.config(), h.connects)
			}
		})
	}
}

func TestEnrollDeviceCancelMidPollReturnsPromptly(t *testing.T) {
	h := newEnrollHarness(t, 1<<30) // pending until cancelled

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		result *enrollResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := h.run(ctx)
		done <- outcome{result, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for h.mock.GetPollCount() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("enrollment never polled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancelled := time.Now()
	cancel()
	select {
	case o := <-done:
		// The mock asks for a 1s interval; a cancel must not wait it out.
		if elapsed := time.Since(cancelled); elapsed > 900*time.Millisecond {
			t.Fatalf("cancel took %s", elapsed)
		}
		if o.result != nil || enrollKind(t, o.err) != enrollCanceled || !errors.Is(o.err, context.Canceled) {
			t.Fatalf("want canceled, got result=%+v err=%v", o.result, o.err)
		}
		h.assertNoGrantSecrets(o.result, o.err)
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not end the enrollment")
	}
	if h.config() != nil || len(h.connects) != 0 {
		t.Fatalf("a cancelled enrollment must save and connect nothing")
	}
}

func seedOperatorKey(t *testing.T, configDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("aceteam_api_key: operator-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestEnrollDeviceLinkRefusalRollsBackThisGrant(t *testing.T) {
	h := newEnrollHarness(t, 1)
	seedOperatorKey(t, h.configDir)
	linker := &scriptedDeviceLinker{linkErrors: []error{&nexus.DeviceLinkRefusedError{StatusCode: http.StatusForbidden}}, version: "1.520.0"}
	h.linker = linker

	result, err := h.run(context.Background())
	if result != nil || enrollKind(t, err) != enrollLinkFailed {
		t.Fatalf("want link_failed, got result=%+v err=%v", result, err)
	}
	h.assertNoGrantSecrets(result, err)
	// The link used this enrollment's own grant and registered node.
	if fmt.Sprint(linker.grants) != "["+enrollTestDeviceCode+"]" || fmt.Sprint(linker.nodeIDs) != "[42]" {
		t.Fatalf("link grants=%v nodes=%v", linker.grants, linker.nodeIDs)
	}
	config := h.config()
	for _, key := range []string{"device_api_token", "org_id", "org_name", "user_email", "api_base_url"} {
		if _, ok := config[key]; ok {
			t.Fatalf("refused enrollment left %s behind: %v", key, config)
		}
	}
	if config["aceteam_api_key"] != "operator-key" {
		t.Fatalf("rollback removed an operator key: %v", config)
	}
	if h.disconnects != 1 {
		t.Fatalf("a refused link must disconnect, got %d", h.disconnects)
	}
}

func TestEnrollDeviceCancelAfterSaveRollsBackBeforeReclaim(t *testing.T) {
	h := newEnrollHarness(t, 1)
	seedOperatorKey(t, h.configDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.deps.saveDeviceConfig = func(dir string, token *nexus.TokenResponse) error {
		err := saveDeviceConfigToDir(dir, token)
		cancel() // the user cancels just after approving
		return err
	}

	_, err := h.run(ctx)
	if enrollKind(t, err) != enrollCanceled {
		t.Fatalf("want canceled, got %v", err)
	}
	config := h.config()
	if _, ok := config["device_api_token"]; ok || config["aceteam_api_key"] != "operator-key" {
		t.Fatalf("a cancel after the save must roll back this grant only: %v", config)
	}
	if len(h.reclaims) != 0 || len(h.connects) != 0 {
		t.Fatalf("a cancel must stop before reclaim and connect: reclaims=%v connects=%v", h.reclaims, h.connects)
	}
}

func TestEnrollDeviceUncertainLinkKeepsCredentials(t *testing.T) {
	prevDelay := deviceLinkRetryDelay
	deviceLinkRetryDelay = 0
	t.Cleanup(func() { deviceLinkRetryDelay = prevDelay })

	h := newEnrollHarness(t, 1)
	h.linker = &scriptedDeviceLinker{linkErrors: []error{nexus.NewDeviceLinkFailure(nexus.DeviceLinkHTTPServer, http.StatusBadGateway)}, version: "1.520.0"}

	_, err := h.run(context.Background())
	if enrollKind(t, err) != enrollLinkFailed {
		t.Fatalf("want link_failed, got %v", err)
	}
	if h.config()["device_api_token"] != enrollTestAPIToken {
		t.Fatalf("an unconfirmed link may have committed; credentials must stay: %v", h.config())
	}
}

func TestEnrollDeviceConnectFailureRollsBackThisGrant(t *testing.T) {
	h := newEnrollHarness(t, 1)
	seedOperatorKey(t, h.configDir)
	h.deps.connect = func(enrollConnectRequest) error { return errors.New("persist node session mode: disk full") }

	_, err := h.run(context.Background())
	if enrollKind(t, err) != enrollConnectFailed {
		t.Fatalf("want connect_failed, got %v", err)
	}
	config := h.config()
	if _, ok := config["device_api_token"]; ok || config["aceteam_api_key"] != "operator-key" {
		t.Fatalf("a grant whose link was never sent must be rolled back, operator keys kept: %v", config)
	}
	// A failure after the network accepted the key must not leave it connected.
	if h.disconnects != 1 || h.links != 0 {
		t.Fatalf("connect failure must disconnect and never link: disconnects=%d links=%d", h.disconnects, h.links)
	}
}

func TestEnrollDeviceConnectErrorNeverEchoesTheAuthkey(t *testing.T) {
	h := newEnrollHarness(t, 1)
	h.deps.connect = func(r enrollConnectRequest) error {
		return fmt.Errorf("control server rejected key %q (also tried tskey-auth-someotherkey)", r.AuthKey)
	}

	result, err := h.run(context.Background())
	if enrollKind(t, err) != enrollConnectFailed {
		t.Fatalf("want connect_failed, got %v", err)
	}
	h.assertNoGrantSecrets(result, err)
	if strings.Contains(err.Error(), "tskey-") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("connect error not redacted: %v", err)
	}
	var ee *enrollError
	if !errors.As(err, &ee) || strings.Contains(fmt.Sprintf("%+v", ee.Err), enrollTestAuthkey) || errors.Unwrap(ee.Err) != nil {
		t.Fatalf("the unredacted cause must not be reachable: %#v", ee)
	}
}

func TestEnrollDeviceRefusesEnrolledMachine(t *testing.T) {
	h := newEnrollHarness(t, 1)
	h.deps.hasState = func() bool { return true }

	_, err := h.run(context.Background())
	if enrollKind(t, err) != enrollAlreadyEnrolled || h.mock.GetPollCount() != 0 || len(h.codes) != 0 {
		t.Fatalf("want already_enrolled before any flow, got err=%v polls=%d", err, h.mock.GetPollCount())
	}
}

func TestEnrollDeviceReplaceRefusedWhileMeshHeld(t *testing.T) {
	h := newEnrollHarness(t, 1)
	h.opts.ForceNew = true
	h.deps.hasState = func() bool { return true }
	h.deps.replacePrecheck = func() error { return errors.New("a citadel worker is running on this host") }

	_, err := h.run(context.Background())
	if enrollKind(t, err) != enrollReplaceRefused || h.mock.GetPollCount() != 0 {
		t.Fatalf("want replace_refused before any flow, got err=%v polls=%d", err, h.mock.GetPollCount())
	}
}

// seedPriorEnrollment writes an earlier enrollment's credentials plus an
// operator key, as a machine enrolled before (or logged out since) holds them.
func seedPriorEnrollment(t *testing.T, configDir string) {
	t.Helper()
	prior := "aceteam_api_key: operator-key\ndevice_api_token: prior-device-token\norg_id: prior-org\norg_name: Prior Org\napi_base_url: https://prior.example.test\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(prior), 0600); err != nil {
		t.Fatal(err)
	}
}

func assertPriorEnrollmentRestored(t *testing.T, config map[string]interface{}) {
	t.Helper()
	for key, want := range map[string]string{
		"aceteam_api_key":  "operator-key",
		"device_api_token": "prior-device-token",
		"org_id":           "prior-org",
		"org_name":         "Prior Org",
		"api_base_url":     "https://prior.example.test",
	} {
		if config[key] != want {
			t.Fatalf("%s = %v, want %q restored: %v", key, config[key], want, config)
		}
	}
	if _, ok := config["user_email"]; ok {
		t.Fatalf("this grant's user_email survived the rollback: %v", config)
	}
}

func TestEnrollDeviceReplaceRestoresPriorCredentialsOnRefusal(t *testing.T) {
	h := newEnrollHarness(t, 1)
	seedPriorEnrollment(t, h.configDir)
	h.opts.ForceNew = true
	h.deps.hasState = func() bool { return true }
	h.linker = &scriptedDeviceLinker{linkErrors: []error{&nexus.DeviceLinkRefusedError{StatusCode: http.StatusForbidden}}, version: "1.520.0"}

	_, err := h.run(context.Background())
	if enrollKind(t, err) != enrollLinkFailed {
		t.Fatalf("want link_failed, got %v", err)
	}
	if len(h.connectReqs) != 1 || !h.connectReqs[0].ForceLogin {
		t.Fatalf("replacing a saved registration must force the new key: %+v", h.connectReqs)
	}
	assertPriorEnrollmentRestored(t, h.config())
}

func TestEnrollDeviceAfterLogoutRestoresPriorCredentialsOnRefusal(t *testing.T) {
	// Logout clears the mesh state but keeps config.yaml, so this machine has
	// no registration yet still holds an earlier grant's token.
	h := newEnrollHarness(t, 1)
	seedPriorEnrollment(t, h.configDir)
	h.linker = &scriptedDeviceLinker{linkErrors: []error{&nexus.DeviceLinkRefusedError{StatusCode: http.StatusForbidden}}, version: "1.520.0"}

	_, err := h.run(context.Background())
	if enrollKind(t, err) != enrollLinkFailed {
		t.Fatalf("want link_failed, got %v", err)
	}
	if h.connectReqs[0].ForceLogin {
		t.Fatal("no saved registration: the login must not be forced")
	}
	assertPriorEnrollmentRestored(t, h.config())
}

func TestEnrollDeviceCancelAfterConnectReturnsCanceled(t *testing.T) {
	h := newEnrollHarness(t, 1)
	seedOperatorKey(t, h.configDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.deps.connect = func(enrollConnectRequest) error {
		cancel() // the user cancels while the host registers
		return nil
	}

	result, err := h.run(ctx)
	if result != nil || enrollKind(t, err) != enrollCanceled {
		t.Fatalf("want canceled, got result=%+v err=%v", result, err)
	}
	if h.links != 0 || h.disconnects != 1 {
		t.Fatalf("a cancel before the link must disconnect and never link: links=%d disconnects=%d", h.links, h.disconnects)
	}
	if _, ok := h.config()["device_api_token"]; ok {
		t.Fatalf("an unlinked grant must be rolled back: %v", h.config())
	}
}

func TestEnrollDeviceIsSingleFlight(t *testing.T) {
	h := newEnrollHarness(t, 1<<30) // the first enrollment waits for approval
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.run(ctx)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for h.mock.GetPollCount() < 1 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("first enrollment never polled")
		}
		time.Sleep(5 * time.Millisecond)
	}

	second := newEnrollHarness(t, 1)
	_, err := second.run(context.Background())
	if enrollKind(t, err) != enrollBusy || second.mock.GetPollCount() != 0 {
		t.Fatalf("a concurrent enrollment must be refused, got %v", err)
	}

	cancel()
	if err := <-done; enrollKind(t, err) != enrollCanceled {
		t.Fatalf("first enrollment: %v", err)
	}
	// The slot is free again once the first enrollment returns.
	if _, err := second.run(context.Background()); err != nil {
		t.Fatalf("enrollment after the first finished: %v", err)
	}
}

func TestDefaultEnrollDepsRefuseForeignDirsAndAuthURL(t *testing.T) {
	if _, err := defaultEnrollDeps(enrollOptions{ConfigDir: t.TempDir(), AuthServiceURL: authServiceURL}); enrollKind(t, err) != enrollMisconfigured {
		t.Fatalf("a config dir other than this node's must be refused: %v", err)
	}
	if _, err := defaultEnrollDeps(enrollOptions{ConfigDir: nodeConfigDirFn(), AuthServiceURL: "https://other.example.test"}); enrollKind(t, err) != enrollMisconfigured {
		t.Fatalf("an auth service other than this process's must be refused: %v", err)
	}
	if _, err := defaultEnrollDeps(enrollOptions{ConfigDir: nodeConfigDirFn(), AuthServiceURL: authServiceURL}); err != nil {
		t.Fatalf("matching options: %v", err)
	}
}

func TestEnrollDeviceAlreadyConnectedStartsNothing(t *testing.T) {
	h := newEnrollHarness(t, 1)
	h.deps.isConnected = func() bool { return true }

	_, err := h.run(context.Background())
	if enrollKind(t, err) != enrollAlreadyConnected || h.mock.GetPollCount() != 0 || len(h.codes) != 0 {
		t.Fatalf("want already_connected before any flow, got err=%v polls=%d codes=%v", err, h.mock.GetPollCount(), h.codes)
	}
}

func TestEnrollDeviceRequiresConfigDir(t *testing.T) {
	h := newEnrollHarness(t, 1)
	h.opts.ConfigDir = ""
	if _, err := h.run(context.Background()); err == nil || h.mock.GetPollCount() != 0 {
		t.Fatalf("an enrollment without an explicit config dir must refuse up front: %v", err)
	}
}
