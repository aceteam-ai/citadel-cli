package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
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

// linkGateServer stands in for an aceteam auth service. The link route always
// refuses with 404 (what a pre-v1.520 server returns for a grant approved
// without the onboarding wizard); version is what /api/version reports, or ""
// for an unavailable version endpoint.
func linkGateServer(t *testing.T, version string, versionCalls *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/fabric/device-config/private-grant/link":
			http.Error(w, `{"error":"No configuration found for this device"}`, http.StatusNotFound)
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
	err      error
	cleared  bool
	warnings []string
}

func runLinkGate(t *testing.T, client deviceLinker) linkGateObservation {
	t.Helper()
	var obs linkGateObservation
	prevClear, prevWarn := clearDeviceCredentialsFn, deviceLinkWarnFn
	t.Cleanup(func() { clearDeviceCredentialsFn, deviceLinkWarnFn = prevClear, prevWarn })
	clearDeviceCredentialsFn = func() { obs.cleared = true }
	deviceLinkWarnFn = func(msg string) { obs.warnings = append(obs.warnings, msg) }
	result := &DeviceAuthResult{DeviceCode: "private-grant", Token: &nexus.TokenResponse{Authkey: "registration-key"}}
	obs.err = linkDeviceEnrollmentGated(result, func(context.Context) string { return "42" }, client)
	for _, w := range obs.warnings {
		if strings.Contains(w, "private-grant") || strings.Contains(w, "registration-key") {
			t.Fatalf("warning exposes enrollment secrets: %q", w)
		}
	}
	return obs
}

func TestDeviceLinkRefusalFromOldServerContinues(t *testing.T) {
	calls := 0
	srv := linkGateServer(t, "1.519.0", &calls)
	obs := runLinkGate(t, nexus.NewDeviceAuthClient(srv.URL))
	if obs.err != nil || obs.cleared || len(obs.warnings) != 1 || calls != 1 {
		t.Fatalf("pre-v1.520 refusal must warn and continue with credentials intact: %+v calls=%d", obs, calls)
	}
}

func TestDeviceLinkRefusalFromEnforcingServerIsFatal(t *testing.T) {
	for _, version := range []string{"1.520.0", "v1.520.0", "1.521.3", "2.0.0"} {
		t.Run(version, func(t *testing.T) {
			calls := 0
			srv := linkGateServer(t, version, &calls)
			obs := runLinkGate(t, nexus.NewDeviceAuthClient(srv.URL))
			if obs.err == nil || !obs.cleared || len(obs.warnings) != 0 {
				t.Fatalf("v1.520+ refusal must fail enrollment and remove saved credentials: %+v", obs)
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
			calls := 0
			srv := linkGateServer(t, version, &calls)
			obs := runLinkGate(t, nexus.NewDeviceAuthClient(srv.URL))
			if obs.err != nil || obs.cleared || len(obs.warnings) != 1 || calls != 1 {
				t.Fatalf("unreadable version must warn and continue: %+v calls=%d", obs, calls)
			}
		})
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
	if obs.err != nil || obs.cleared || len(obs.warnings) != 0 || l.versionCalls != 0 {
		t.Fatalf("a confirmed link needs no version gate: %+v calls=%d", obs, l.versionCalls)
	}
}
