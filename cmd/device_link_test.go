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
	if strings.Contains(err.Error(), refused.Error()) {
		t.Fatal("recovery diagnostic must not render its arbitrary cause")
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
	return runLinkGateWithRollback(t, client, nil)
}

func runLinkGateWithRollback(t *testing.T, client deviceLinker, rollback func(*nexus.TokenResponse) error) linkGateObservation {
	t.Helper()
	var obs linkGateObservation
	prevRollback, prevWarn, prevDelay := enrollmentRollbackFn, deviceLinkWarnFn, deviceLinkRetryDelay
	t.Cleanup(func() {
		enrollmentRollbackFn, deviceLinkWarnFn, deviceLinkRetryDelay = prevRollback, prevWarn, prevDelay
	})
	enrollmentRollbackFn = func(token *nexus.TokenResponse) error {
		obs.rolledBack = token
		if rollback != nil {
			return rollback(token)
		}
		return nil
	}
	deviceLinkWarnFn = func(msg string) { obs.warnings = append(obs.warnings, msg) }
	deviceLinkRetryDelay = 0
	result := &DeviceAuthResult{DeviceCode: "private-grant", Token: linkGateToken}
	obs.err = linkDeviceEnrollmentGated(result, func(context.Context) string { return "42" }, client)
	diagnostics := append([]string(nil), obs.warnings...)
	if obs.err != nil {
		diagnostics = append(diagnostics, obs.err.Error())
	}
	for _, w := range diagnostics {
		if strings.Contains(w, "private-grant") || strings.Contains(w, "registration-key") || strings.Contains(w, "device-bearer") {
			t.Fatalf("diagnostic exposes enrollment secrets: %q", w)
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

func TestDeviceLinkTransientFailureFromEnforcingServerFailsWithoutRollback(t *testing.T) {
	links, versions := 0, 0
	srv := linkGateServer(t, "1.520.0", []int{502}, &links, &versions)
	obs := runLinkGate(t, nexus.NewDeviceAuthClient(srv.URL))
	if obs.err == nil || obs.rolledBack != nil || len(obs.warnings) != 0 || links != 2 || versions != 1 {
		t.Fatalf("unconfirmed v1.520+ link must fail after one retry and retain credentials: %+v links=%d versions=%d", obs, links, versions)
	}
}

type scriptedDeviceLinker struct {
	linkErrors   []error
	version      string
	versionError error
	grants       []string
	nodeIDs      []string
	versionCalls int
}

func (l *scriptedDeviceLinker) LinkRegisteredNode(_ context.Context, grant, nodeID string) error {
	l.grants = append(l.grants, grant)
	l.nodeIDs = append(l.nodeIDs, nodeID)
	return l.linkErrors[min(len(l.grants)-1, len(l.linkErrors)-1)]
}

func (l *scriptedDeviceLinker) ServerVersion(context.Context) (string, error) {
	l.versionCalls++
	return l.version, l.versionError
}

// No diagnostic may call Error on an opaque cause: it may contain credentials,
// grant-bearing URLs, or unsafe terminal text supplied by a transport.
type opaqueDeviceLinkError struct{ rendered bool }

func (e *opaqueDeviceLinkError) Error() string {
	e.rendered = true
	panic("opaque link error must not be rendered")
}

func TestDeviceLinkFinalOutcomeUsesVersionWithoutReplayingExpiredGrants(t *testing.T) {
	opaque := &opaqueDeviceLinkError{}
	expired := &nexus.DeviceLinkRefusedError{StatusCode: http.StatusGone}
	for _, tt := range []struct {
		name         string
		version      string
		versionError error
		linkErrors   []error
		wantFailure  bool
		wantRollback bool
		wantLinks    int
	}{
		{name: "modern transport", version: "1.520.0", linkErrors: []error{opaque}, wantFailure: true, wantLinks: 2},
		{name: "modern expired grant", version: "1.520.0", linkErrors: []error{expired}, wantFailure: true, wantRollback: true, wantLinks: 1},
		{name: "modern transport then refusal", version: "1.521.0", linkErrors: []error{opaque, expired}, wantFailure: true, wantLinks: 2},
		{name: "old transport", version: "1.519.9", linkErrors: []error{opaque}, wantLinks: 2},
		{name: "old expired grant", version: "1.519.9", linkErrors: []error{expired}, wantLinks: 1},
		{name: "unknown transport", versionError: opaque, linkErrors: []error{opaque}, wantLinks: 2},
		{name: "unknown expired grant", versionError: opaque, linkErrors: []error{expired}, wantLinks: 1},
		{name: "unrecognized private version", version: "private-grant registration-key device-bearer", linkErrors: []error{opaque}, wantLinks: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &scriptedDeviceLinker{linkErrors: tt.linkErrors, version: tt.version, versionError: tt.versionError}
			obs := runLinkGate(t, client)
			if opaque.rendered {
				t.Fatal("link and version causes must never be rendered, even by a formatter that recovers panics")
			}
			if (obs.err != nil) != tt.wantFailure || (obs.rolledBack != nil) != tt.wantRollback || len(client.grants) != tt.wantLinks || client.versionCalls != 1 {
				t.Fatalf("unexpected final outcome: failed=%v rollback=%v links=%d versions=%d", obs.err != nil, obs.rolledBack != nil, len(client.grants), client.versionCalls)
			}
			for i, grant := range client.grants {
				if grant != "private-grant" || client.nodeIDs[i] != "42" {
					t.Fatal("link must use only the current grant and current registered identity")
				}
			}
			if tt.wantFailure {
				if len(obs.warnings) != 0 || !errors.Is(obs.err, tt.linkErrors[len(tt.linkErrors)-1]) || !strings.Contains(obs.err.Error(), "fresh browser approval") {
					t.Fatal("modern failure must preserve its typed cause and require fresh approval without a compatibility warning")
				}
			} else if len(obs.warnings) != 1 || !strings.Contains(obs.warnings[0], "unverified") || !strings.Contains(obs.warnings[0], "citadel logout && citadel init") || !strings.Contains(obs.warnings[0], "fresh browser approval") {
				t.Fatal("compatibility must explicitly report unverified binding and fresh-registration recovery")
			}
		})
	}
}

func TestDeviceLinkUncertaintyRetainsSavedCredentialBytes(t *testing.T) {
	for _, tt := range []struct {
		name       string
		linkErrors []error
	}{
		{name: "uncertain twice", linkErrors: []error{&opaqueDeviceLinkError{}}},
		{name: "uncertain then expired", linkErrors: []error{&opaqueDeviceLinkError{}, &nexus.DeviceLinkRefusedError{StatusCode: http.StatusGone}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			content := "device_api_token: device-bearer\norg_id: current-org\naceteam_api_key: operator-key\n"
			path := writeRollbackFixture(t, content)
			client := &scriptedDeviceLinker{linkErrors: tt.linkErrors, version: "1.520.0"}
			obs := runLinkGateWithRollback(t, client, func(token *nexus.TokenResponse) error {
				return rollbackEnrollmentCredentials(path, token)
			})
			if obs.err == nil || obs.rolledBack != nil || len(client.grants) != 2 || client.versionCalls != 1 {
				t.Fatal("uncertain modern link must fail after its bounded retry without attempting credential removal")
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != content {
				t.Fatal("a later refusal cannot disprove an earlier server commit; saved credentials must remain byte-for-byte unchanged")
			}
		})
	}
}

func TestDeviceLinkDefiniteRefusalRollsBackOnlyCurrentCredential(t *testing.T) {
	for _, bearer := range []string{"device-bearer", "replacement-bearer"} {
		t.Run(bearer, func(t *testing.T) {
			path := writeRollbackFixture(t, "device_api_token: "+bearer+"\naceteam_api_key: operator-key\n")
			client := &scriptedDeviceLinker{linkErrors: []error{&nexus.DeviceLinkRefusedError{StatusCode: http.StatusForbidden}}, version: "1.520.0"}
			obs := runLinkGateWithRollback(t, client, func(token *nexus.TokenResponse) error {
				return rollbackEnrollmentCredentials(path, token)
			})
			if obs.err == nil || obs.rolledBack != linkGateToken {
				t.Fatal("definite modern refusal must attempt current-enrollment rollback")
			}
			got := readRollbackFixture(t, path)
			if got["aceteam_api_key"] != "operator-key" {
				t.Fatal("rollback must preserve the operator credential")
			}
			if bearer == "device-bearer" {
				if _, ok := got["device_api_token"]; ok {
					t.Fatal("refused current credential must be removed")
				}
			} else if got["device_api_token"] != bearer {
				t.Fatal("refusal must not remove a replacement credential")
			}
		})
	}
}

func TestDeviceLinkRollbackFailureDiagnosticIsRedacted(t *testing.T) {
	client := &scriptedDeviceLinker{linkErrors: []error{&nexus.DeviceLinkRefusedError{StatusCode: http.StatusForbidden}}, version: "1.520.0"}
	opaque := &opaqueDeviceLinkError{}
	obs := runLinkGateWithRollback(t, client, func(*nexus.TokenResponse) error { return opaque })
	if obs.err == nil || opaque.rendered || !strings.Contains(obs.err.Error(), "removing this enrollment's saved credentials also failed") {
		t.Fatal("rollback failure must remain visible without rendering its opaque cause")
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
	registeredNodeID.Lock()
	previousID := registeredNodeID.id
	registeredNodeID.Unlock()
	t.Cleanup(func() { setRegisteredNodeID(previousID) })
	token := &nexus.TokenResponse{Authkey: "key", DeviceAPIToken: "bearer", OrgID: "current-org", NodeUID: "current-node"}
	want := *token
	links := 0
	err := ccConnectDeviceEnrollmentWith(token, "grant", func() error {
		// Registration must not leave the later link aliased to its caller.
		token.Authkey = "replacement-key"
		token.DeviceAPIToken = "replacement-bearer"
		return nil
	}, func(result *DeviceAuthResult) error {
		links++
		if result.DeviceCode != "grant" || result.Token == token || *result.Token != want {
			t.Fatal("link lost the complete current response or retained its mutable alias")
		}
		return nil
	}, func() error { t.Fatal("successful link must not disconnect"); return nil })
	if err != nil || links != 1 {
		t.Fatal("direct current response did not link exactly once")
	}
}

func TestControlCenterLinkNeverSubstitutesAMissingToken(t *testing.T) {
	for _, token := range []*nexus.TokenResponse{nil, {}, {DeviceAPIToken: "saved-bearer-alone"}} {
		err := ccConnectDeviceEnrollmentWith(token, "grant", func() error {
			t.Fatal("missing current response must fail before registration")
			return nil
		}, func(*DeviceAuthResult) error {
			t.Fatal("must not synthesize a token or inherit another enrollment")
			return nil
		}, func() error { t.Fatal("nothing was connected"); return nil })
		if err == nil {
			t.Fatal("missing current token was accepted")
		}
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
