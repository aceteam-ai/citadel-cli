package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"gopkg.in/yaml.v3"
)

func TestDeviceEnrollmentLinkUsesRegisteredIdentityAndCurrentGrant(t *testing.T) {
	result := &DeviceAuthResult{DeviceCode: "current-grant", Token: &nexus.TokenResponse{Authkey: "current-registration-key"}}
	nodeID := func(context.Context) string { return "42" }
	called := false
	refused := errors.New("reservation no longer owns this node")
	err := linkDeviceEnrollmentWith(context.Background(), result, nodeID, func(_ context.Context, code, id string) error {
		called = true
		if code != result.DeviceCode || id != "42" {
			t.Fatal("link substituted another enrollment or node identity")
		}
		return refused
	})
	if !called || !errors.Is(err, refused) {
		t.Fatal("server link refusal must propagate to the enrollment caller")
	}
}

func TestDeviceEnrollmentLinkRefusesMissingCurrentAuthorization(t *testing.T) {
	for _, result := range []*DeviceAuthResult{nil, {}, {Token: &nexus.TokenResponse{}}, {DeviceCode: "grant", Token: &nexus.TokenResponse{DeviceAPIToken: "saved-bearer-alone"}}} {
		err := linkDeviceEnrollmentWith(context.Background(), result, func(context.Context) string { t.Fatal("must reject before network identity lookup"); return "42" }, func(context.Context, string, string) error { t.Fatal("must not link"); return nil })
		if err == nil {
			t.Fatal("missing current authorization must fail")
		}
	}
	result := &DeviceAuthResult{DeviceCode: "grant", Token: &nexus.TokenResponse{Authkey: "registration-key"}}
	err := linkDeviceEnrollmentWith(context.Background(), result, func(context.Context) string { return "" }, func(context.Context, string, string) error {
		t.Fatal("must not substitute hostname or IP")
		return nil
	})
	if err == nil {
		t.Fatal("missing registered identity must fail")
	}
}

func TestDeviceEnrollmentRefusalExplainsFreshRegistrationRecovery(t *testing.T) {
	refused := errors.New("device reservation does not match the connected node")
	err := deviceEnrollmentRecoveryError(refused)
	if !errors.Is(err, refused) || !strings.Contains(err.Error(), "saved credentials do not complete device binding") || !strings.Contains(err.Error(), "citadel logout && citadel init") {
		t.Fatal("link refusal must preserve its cause and explain explicit fresh-registration recovery")
	}
}

// linkGateServer stands in for an aceteam auth service. linkStatuses are the
// link route's successive answers (the last repeats): 404 is what a pre-v1.520
// server returns for a grant approved without the onboarding wizard. version is
// what /api/version reports, or "" for an unavailable version endpoint.
func linkGateServer(t *testing.T, version string, linkStatuses []int, linkCalls, versionCalls *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/fabric/device-config/private-grant/link":
			status := linkStatuses[min(*linkCalls, len(linkStatuses)-1)]
			*linkCalls++
			if status == http.StatusOK {
				fmt.Fprint(w, `{"success":true}`)
				return
			}
			http.Error(w, `{"error":"No configuration found for this device"}`, status)
		case "/api/version":
			*versionCalls++
			if version == "" {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			fmt.Fprintf(w, `{"version":%q,"service":"web"}`, version)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type linkGateObservation struct {
	err        error
	rolledBack *nexus.TokenResponse
	warnings   []string
}

var linkGateToken = &nexus.TokenResponse{Authkey: "registration-key", DeviceAPIToken: "device-bearer"}

func runLinkGate(t *testing.T, client deviceLinker) linkGateObservation {
	t.Helper()
	var obs linkGateObservation
	prevRollback, prevWarn, prevDelay := enrollmentRollbackFn, deviceLinkWarnFn, deviceLinkRetryDelay
	t.Cleanup(func() {
		enrollmentRollbackFn, deviceLinkWarnFn, deviceLinkRetryDelay = prevRollback, prevWarn, prevDelay
	})
	enrollmentRollbackFn = func(token *nexus.TokenResponse) error { obs.rolledBack = token; return nil }
	deviceLinkWarnFn = func(msg string) { obs.warnings = append(obs.warnings, msg) }
	deviceLinkRetryDelay = 0
	result := &DeviceAuthResult{DeviceCode: "private-grant", Token: linkGateToken}
	obs.err = linkDeviceEnrollmentGated(result, func(context.Context) string { return "42" }, client)
	for _, w := range obs.warnings {
		if strings.Contains(w, "private-grant") || strings.Contains(w, "registration-key") || strings.Contains(w, "device-bearer") {
			t.Fatalf("warning exposes enrollment secrets: %q", w)
		}
	}
	return obs
}

func TestDeviceLinkRefusalFromOldServerContinues(t *testing.T) {
	for _, version := range []string{"1.519.0", "1.99.0", "0.9.0"} {
		t.Run(version, func(t *testing.T) {
			links, versions := 0, 0
			srv := linkGateServer(t, version, []int{404}, &links, &versions)
			obs := runLinkGate(t, nexus.NewDeviceAuthClient(srv.URL))
			if obs.err != nil || obs.rolledBack != nil || len(obs.warnings) != 1 || links != 1 || versions != 1 {
				t.Fatalf("pre-v1.520 refusal must warn once and keep credentials: %+v links=%d versions=%d", obs, links, versions)
			}
		})
	}
}

func TestDeviceLinkRefusalFromEnforcingServerIsFatal(t *testing.T) {
	for _, version := range []string{"1.520.0", "v1.520.0", "1.521.3", "1.1000.0", "2.0.0"} {
		t.Run(version, func(t *testing.T) {
			links, versions := 0, 0
			srv := linkGateServer(t, version, []int{404}, &links, &versions)
			obs := runLinkGate(t, nexus.NewDeviceAuthClient(srv.URL))
			if obs.err == nil || obs.rolledBack != linkGateToken || len(obs.warnings) != 0 || links != 1 {
				t.Fatalf("v1.520+ refusal must fail and roll back this enrollment (no retry): %+v links=%d", obs, links)
			}
			if !strings.Contains(obs.err.Error(), "citadel logout && citadel init") {
				t.Fatalf("fatal refusal must explain recovery: %v", obs.err)
			}
		})
	}
}

func TestDeviceLinkRefusalWithUnreadableVersionContinues(t *testing.T) {
	for _, version := range []string{"", "not-a-version"} {
		t.Run(fmt.Sprintf("version=%q", version), func(t *testing.T) {
			links, versions := 0, 0
			srv := linkGateServer(t, version, []int{404}, &links, &versions)
			obs := runLinkGate(t, nexus.NewDeviceAuthClient(srv.URL))
			if obs.err != nil || obs.rolledBack != nil || len(obs.warnings) != 1 || versions != 1 {
				t.Fatalf("unreadable version must warn and continue: %+v versions=%d", obs, versions)
			}
		})
	}
}

func TestDeviceLinkTransientFailureRetriesOnceThenContinues(t *testing.T) {
	links, versions := 0, 0
	srv := linkGateServer(t, "1.520.0", []int{502}, &links, &versions)
	obs := runLinkGate(t, nexus.NewDeviceAuthClient(srv.URL))
	if obs.err != nil || obs.rolledBack != nil || len(obs.warnings) != 1 || links != 2 || versions != 0 {
		t.Fatalf("a 5xx is not a refusal, even on v1.520+: retry once, then warn: %+v links=%d versions=%d", obs, links, versions)
	}
}

func TestDeviceLinkTransientFailureRecoversOnRetry(t *testing.T) {
	links, versions := 0, 0
	srv := linkGateServer(t, "1.520.0", []int{503, 200}, &links, &versions)
	obs := runLinkGate(t, nexus.NewDeviceAuthClient(srv.URL))
	if obs.err != nil || obs.rolledBack != nil || len(obs.warnings) != 0 || links != 2 {
		t.Fatalf("retry must complete the link: %+v links=%d", obs, links)
	}
}

type successfulLinker struct{ versionCalls int }

func (l *successfulLinker) LinkRegisteredNode(context.Context, string, string) error { return nil }
func (l *successfulLinker) ServerVersion(context.Context) (string, error) {
	l.versionCalls++
	return "1.520.0", nil
}

func TestDeviceLinkSuccessDoesNotConsultVersion(t *testing.T) {
	l := &successfulLinker{}
	obs := runLinkGate(t, l)
	if obs.err != nil || obs.rolledBack != nil || len(obs.warnings) != 0 || l.versionCalls != 0 {
		t.Fatalf("a confirmed link needs no version gate: %+v calls=%d", obs, l.versionCalls)
	}
}

func writeRollbackFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readRollbackFixture(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]interface{}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestRollbackEnrollmentCredentialsRemovesOnlyThisEnrollmentsFields(t *testing.T) {
	path := writeRollbackFixture(t, `device_api_token: new-bearer
api_base_url: https://api.example
org_id: org-new
org_name: Other Name Kept
aceteam_api_key: operator-key
login_node_uid: node-uid
nexus_url: https://nexus.example
`)
	token := &nexus.TokenResponse{DeviceAPIToken: "new-bearer", APIBaseURL: "https://api.example", OrgID: "org-new", OrgName: "Org Name"}
	if err := rollbackEnrollmentCredentials(path, token); err != nil {
		t.Fatal(err)
	}
	got := readRollbackFixture(t, path)
	for _, key := range []string{"device_api_token", "api_base_url", "org_id"} {
		if _, ok := got[key]; ok {
			t.Errorf("%s written by this enrollment must be removed", key)
		}
	}
	for key, want := range map[string]string{"org_name": "Other Name Kept", "aceteam_api_key": "operator-key", "login_node_uid": "node-uid", "nexus_url": "https://nexus.example"} {
		if got[key] != want {
			t.Errorf("%s = %v, want %q kept", key, got[key], want)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("rewritten config must stay 0600: %v %v", info, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".config.yaml.tmp-*")); len(leftovers) != 0 {
		t.Fatalf("atomic write left temp files: %v", leftovers)
	}
}

func TestRollbackEnrollmentCredentialsLeavesFileWhenSaveDidNotLand(t *testing.T) {
	for name, content := range map[string]string{
		"prior enrollment": "device_api_token: prior-bearer\norg_id: org-new\n",
		"unparseable":      "device_api_token: [unclosed\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := writeRollbackFixture(t, content)
			if err := rollbackEnrollmentCredentials(path, &nexus.TokenResponse{DeviceAPIToken: "new-bearer", OrgID: "org-new"}); err != nil {
				t.Fatal(err)
			}
			if data, _ := os.ReadFile(path); string(data) != content {
				t.Fatalf("file must be untouched, got %q", data)
			}
		})
	}
	missing := filepath.Join(t.TempDir(), "config.yaml")
	if err := rollbackEnrollmentCredentials(missing, &nexus.TokenResponse{DeviceAPIToken: "new-bearer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("rollback must not create a config file")
	}
}

func TestRollbackEnrollmentCredentialsRedisMode(t *testing.T) {
	path := writeRollbackFixture(t, "redis_url: redis://new\naceteam_api_key: operator-key\n")
	if err := rollbackEnrollmentCredentials(path, &nexus.TokenResponse{RedisURL: "redis://new"}); err != nil {
		t.Fatal(err)
	}
	got := readRollbackFixture(t, path)
	if _, ok := got["redis_url"]; ok || got["aceteam_api_key"] != "operator-key" {
		t.Fatalf("redis rollback must remove only redis_url: %v", got)
	}
}

func TestControlCenterLinkUsesPolledToken(t *testing.T) {
	token := &nexus.TokenResponse{Authkey: "key", DeviceAPIToken: "bearer"}
	ccEnrollmentTokens.Store("grant", token)
	if got := ccEnrollmentTokenFor("grant", "key"); got != token {
		t.Fatal("connect must roll back with the token its poll saved")
	}
	if got := ccEnrollmentTokenFor("grant", "key"); got == token || got.DeviceAPIToken != "" {
		t.Fatal("the stored token is consumed once")
	}
	ccEnrollmentTokens.Store("grant", token)
	if got := ccEnrollmentTokenFor("grant", "other-key"); got == token {
		t.Fatal("a token for a different authorization must not be used")
	}
}

func TestEnrollmentNodeIDUsesThisProcessRegistrationAfterDisconnect(t *testing.T) {
	t.Cleanup(func() { setRegisteredNodeID("") })
	setRegisteredNodeID("")
	if id := enrollmentNodeID(context.Background()); id != "" {
		t.Fatalf("no registration yet, got %q", id)
	}
	// connectToNetwork records the ID and then disconnects, so the global
	// server is gone; the link must still see the registered identity.
	setRegisteredNodeID("1234")
	if id := enrollmentNodeID(context.Background()); id != "1234" {
		t.Fatalf("enrollment node ID after disconnect = %q, want the registered 1234", id)
	}
}

func TestRecordRegistrationThenDisconnectRecordsBeforeDisconnect(t *testing.T) {
	t.Cleanup(func() { setRegisteredNodeID("") })
	setRegisteredNodeID("")
	var order []string
	status := func(context.Context) (*network.NetworkStatus, error) {
		order = append(order, "status")
		return &network.NetworkStatus{NodeID: "1879"}, nil
	}
	disconnect := func() error {
		order = append(order, "disconnect")
		registeredNodeID.Lock()
		got := registeredNodeID.id
		registeredNodeID.Unlock()
		if got != "1879" {
			t.Errorf("node ID at disconnect = %q, want 1879 recorded first", got)
		}
		return nil
	}
	recordRegistrationThenDisconnect(context.Background(), status, disconnect)
	if strings.Join(order, ",") != "status,disconnect" {
		t.Fatalf("call order = %v, want status before disconnect", order)
	}
}

func TestRecordRegistrationThenDisconnectStatusErrorStillDisconnects(t *testing.T) {
	t.Cleanup(func() { setRegisteredNodeID("") })
	setRegisteredNodeID("")
	disconnected := false
	recordRegistrationThenDisconnect(context.Background(),
		func(context.Context) (*network.NetworkStatus, error) { return nil, errors.New("no status") },
		func() error { disconnected = true; return nil })
	if !disconnected {
		t.Fatal("a status failure must still disconnect")
	}
	if id := enrollmentNodeID(context.Background()); id != "" {
		t.Fatalf("no ID may be recorded when status fails, got %q", id)
	}
}

func TestControlCenterTokenPollRetriesServerErrorsBounded(t *testing.T) {
	t.Cleanup(func() { ccTokenServerErrors.Delete("grant") })
	serverErr := &nexus.TokenServerError{StatusCode: 500}
	for i := 1; i <= nexus.MaxConsecutiveTokenServerErrors; i++ {
		if err := ccTokenPollError("grant", serverErr); !strings.Contains(err.Error(), "pending") {
			t.Fatalf("5xx #%d must keep the modal polling, got %v", i, err)
		}
	}
	if err := ccTokenPollError("grant", serverErr); err != serverErr {
		t.Fatalf("too many 5xx in a row must end the modal, got %v", err)
	}
	other := errors.New("access denied")
	if err := ccTokenPollError("grant", other); err != other {
		t.Fatalf("non-5xx errors pass through unchanged, got %v", err)
	}
}
