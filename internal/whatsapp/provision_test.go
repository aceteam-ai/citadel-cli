package whatsapp

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeBridge is an in-memory BridgeClient for provision tests.
type fakeBridge struct {
	ready       error
	created     *Tenant
	createErr   error
	createCalls int
	health      *Health
	healthErr   error
	qr          string
	qrErr       error
}

func (f *fakeBridge) WaitReady(ctx context.Context, timeout time.Duration) error { return f.ready }
func (f *fakeBridge) CreateTenant(ctx context.Context, name, proxyURL string) (*Tenant, error) {
	f.createCalls++
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.created != nil {
		return f.created, nil
	}
	return &Tenant{ID: "t_1", Name: name, APIKey: "wab_minted"}, nil
}
func (f *fakeBridge) Health(ctx context.Context, apiKey string) (*Health, error) {
	return f.health, f.healthErr
}
func (f *fakeBridge) QRString(ctx context.Context, apiKey string) (string, error) {
	return f.qr, f.qrErr
}

// baseDeps builds ProvisionDeps wired to the given fake bridge and a temp
// services dir, with a reachable mesh URL and a no-op deploy. Individual tests
// override fields.
func baseDeps(t *testing.T, bridge *fakeBridge) (ProvisionDeps, *bool) {
	t.Helper()
	dir := t.TempDir()
	deployed := false
	deps := ProvisionDeps{
		ServicesDir:   func() (string, error) { return dir, nil },
		DeployCompose: func(servicesDir string, env map[string]string) error { deployed = true; return nil },
		NewBridgeClient: func(port int, adminKey string) BridgeClient {
			return bridge
		},
		MeshAPIURL: func(port int) string { return "http://100.64.0.7:8080" },
	}
	return deps, &deployed
}

func TestProvisionHappyPath(t *testing.T) {
	bridge := &fakeBridge{
		health: &Health{LoggedIn: false},
		qr:     "2@qr-payload",
	}
	deps, deployed := baseDeps(t, bridge)

	res, err := Provision(context.Background(), ProvisionRequest{}, deps)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if !*deployed {
		t.Error("expected DeployCompose to be called")
	}
	if res.APIURL != "http://100.64.0.7:8080" {
		t.Errorf("api_url = %q, want mesh IP url", res.APIURL)
	}
	if res.APIKey != "wab_minted" {
		t.Errorf("api_key = %q, want minted key", res.APIKey)
	}
	if res.Tenant != "default" {
		t.Errorf("tenant = %q, want default", res.Tenant)
	}
	if res.QR != "2@qr-payload" {
		t.Errorf("qr = %q, want payload", res.QR)
	}
	if res.AlreadyLinked {
		t.Error("AlreadyLinked = true, want false")
	}
	if bridge.createCalls != 1 {
		t.Errorf("CreateTenant calls = %d, want 1", bridge.createCalls)
	}
}

func TestProvisionDefaultsTenant(t *testing.T) {
	bridge := &fakeBridge{health: &Health{}, qr: "x"}
	deps, _ := baseDeps(t, bridge)
	res, err := Provision(context.Background(), ProvisionRequest{Tenant: ""}, deps)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if res.Tenant != "default" {
		t.Errorf("tenant defaulted to %q, want default", res.Tenant)
	}
}

func TestProvisionAlreadyLinkedViaHealth(t *testing.T) {
	bridge := &fakeBridge{health: &Health{LoggedIn: true}, qr: "should-not-be-returned"}
	deps, _ := baseDeps(t, bridge)
	res, err := Provision(context.Background(), ProvisionRequest{}, deps)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if !res.AlreadyLinked {
		t.Error("AlreadyLinked = false, want true (health says logged in)")
	}
	if res.QR != "" {
		t.Errorf("qr = %q, want empty for already-linked tenant", res.QR)
	}
	if res.APIKey == "" || res.APIURL == "" {
		t.Error("already-linked result must still carry api_url + api_key")
	}
}

func TestProvisionAlreadyLinkedViaEmptyQR(t *testing.T) {
	// health not logged-in, but the bridge returns an empty QR -> also linked.
	bridge := &fakeBridge{health: &Health{LoggedIn: false}, qr: ""}
	deps, _ := baseDeps(t, bridge)
	res, err := Provision(context.Background(), ProvisionRequest{}, deps)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if !res.AlreadyLinked {
		t.Error("empty QR should be treated as already-linked")
	}
}

func TestProvisionOffMeshFails(t *testing.T) {
	bridge := &fakeBridge{health: &Health{}, qr: "x"}
	deps, deployed := baseDeps(t, bridge)
	deps.MeshAPIURL = func(port int) string { return "" } // off-mesh

	_, err := Provision(context.Background(), ProvisionRequest{}, deps)
	if err == nil {
		t.Fatal("expected an error when off-mesh, got nil")
	}
	if !strings.Contains(err.Error(), "mesh") {
		t.Errorf("error = %q, want it to mention the mesh", err.Error())
	}
	if *deployed {
		t.Error("must not deploy the bridge when the node is off-mesh")
	}
}

func TestProvisionDeployErrorPropagates(t *testing.T) {
	bridge := &fakeBridge{health: &Health{}, qr: "x"}
	deps, _ := baseDeps(t, bridge)
	deps.DeployCompose = func(string, map[string]string) error {
		return errors.New("docker daemon not running")
	}
	_, err := Provision(context.Background(), ProvisionRequest{}, deps)
	if err == nil || !strings.Contains(err.Error(), "docker daemon") {
		t.Fatalf("expected docker error to propagate, got %v", err)
	}
}

func TestProvisionReusesExistingTenant(t *testing.T) {
	bridge := &fakeBridge{health: &Health{LoggedIn: false}, qr: "q"}
	deps, _ := baseDeps(t, bridge)
	dir, _ := deps.ServicesDir()
	// Seed a stored tenant key so Provision reuses it and never mints anew.
	if err := SaveEnv(dir, map[string]string{"ADMIN_API_KEY": "adm", "TENANT_API_KEY": "wab_existing"}); err != nil {
		t.Fatal(err)
	}
	res, err := Provision(context.Background(), ProvisionRequest{}, deps)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if bridge.createCalls != 0 {
		t.Errorf("CreateTenant called %d times, want 0 (should reuse)", bridge.createCalls)
	}
	if res.APIKey != "wab_existing" {
		t.Errorf("api_key = %q, want the reused key", res.APIKey)
	}
}

// TestProvisionExposesGatewayRouteWithBridgePort verifies Provision wires the
// gateway route to the bridge's chosen host port (aceteam-ai/citadel-cli#447).
func TestProvisionExposesGatewayRouteWithBridgePort(t *testing.T) {
	bridge := &fakeBridge{health: &Health{LoggedIn: false}, qr: "q"}
	deps, _ := baseDeps(t, bridge)
	// Pin an explicit port so the assertion is deterministic (operator override
	// is honored verbatim).
	var exposedPort int
	exposeCalls := 0
	deps.ExposeGatewayRoute = func(bridgePort int) error {
		exposeCalls++
		exposedPort = bridgePort
		return nil
	}
	if _, err := Provision(context.Background(), ProvisionRequest{Port: 8137}, deps); err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if exposeCalls != 1 {
		t.Fatalf("ExposeGatewayRoute calls = %d, want 1", exposeCalls)
	}
	if exposedPort != 8137 {
		t.Errorf("exposed bridge port = %d, want 8137 (the published host port)", exposedPort)
	}
}

// TestProvisionExposeGatewayRouteErrorIsFatal verifies a route-exposure failure
// fails the provision loud rather than returning a false-green with an
// unreachable api_url.
func TestProvisionExposeGatewayRouteErrorIsFatal(t *testing.T) {
	bridge := &fakeBridge{health: &Health{LoggedIn: false}, qr: "q"}
	deps, _ := baseDeps(t, bridge)
	deps.ExposeGatewayRoute = func(int) error { return errors.New("no gateway running") }
	_, err := Provision(context.Background(), ProvisionRequest{}, deps)
	if err == nil || !strings.Contains(err.Error(), "gateway") {
		t.Fatalf("expected a loud gateway-exposure error, got %v", err)
	}
}

// TestProvisionVerifyReachableFailsBeforeAlreadyLinked is the core #447 guard:
// when the mesh api_url is NOT reachable, Provision must fail loud EVEN IF the
// tenant is already linked (the false-green we hit). The health probe says
// logged-in, but the reachability check must run first and sink the provision.
func TestProvisionVerifyReachableFailsBeforeAlreadyLinked(t *testing.T) {
	bridge := &fakeBridge{health: &Health{LoggedIn: true}, qr: ""}
	deps, _ := baseDeps(t, bridge)
	verifyCalls := 0
	deps.VerifyReachable = func(ctx context.Context, apiURL string) error {
		verifyCalls++
		return errors.New("HTTP 502 (bridge not exposed on the mesh)")
	}
	res, err := Provision(context.Background(), ProvisionRequest{}, deps)
	if err == nil {
		t.Fatalf("expected a loud unreachable error, got success res=%+v", res)
	}
	if verifyCalls != 1 {
		t.Errorf("VerifyReachable calls = %d, want 1 (must run before already_linked)", verifyCalls)
	}
	if !strings.Contains(err.Error(), "reachable") {
		t.Errorf("error = %q, want it to explain the bridge is not reachable", err.Error())
	}
}

// TestProvisionVerifyReachableReceivesMeshAPIURL asserts the reachability probe
// is handed the same api_url the result advertises (so the check is meaningful).
func TestProvisionVerifyReachableReceivesMeshAPIURL(t *testing.T) {
	bridge := &fakeBridge{health: &Health{LoggedIn: false}, qr: "q"}
	deps, _ := baseDeps(t, bridge)
	deps.MeshAPIURL = func(port int) string { return "https://100.64.0.7:8443/whatsapp" }
	var gotURL string
	deps.VerifyReachable = func(ctx context.Context, apiURL string) error {
		gotURL = apiURL
		return nil
	}
	res, err := Provision(context.Background(), ProvisionRequest{}, deps)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if gotURL != "https://100.64.0.7:8443/whatsapp" {
		t.Errorf("VerifyReachable got api_url %q, want the gateway-route URL", gotURL)
	}
	if res.APIURL != "https://100.64.0.7:8443/whatsapp" {
		t.Errorf("result api_url = %q, want the gateway-route URL", res.APIURL)
	}
}

func TestProvisionQRFetchErrorIsNonFatal(t *testing.T) {
	bridge := &fakeBridge{health: &Health{LoggedIn: false}, qrErr: errors.New("transient")}
	deps, _ := baseDeps(t, bridge)
	res, err := Provision(context.Background(), ProvisionRequest{}, deps)
	if err != nil {
		t.Fatalf("a QR-fetch error must not fail the provision, got %v", err)
	}
	if res.QR != "" {
		t.Errorf("qr = %q, want empty after fetch error", res.QR)
	}
	if res.APIKey == "" {
		t.Error("result must still carry the minted api_key")
	}
}

func TestProvisionRequiresDeps(t *testing.T) {
	_, err := Provision(context.Background(), ProvisionRequest{}, ProvisionDeps{})
	if err == nil {
		t.Fatal("expected an error when required deps are missing")
	}
}

func TestQRDataURL(t *testing.T) {
	if got, err := QRDataURL(""); err != nil || got != "" {
		t.Errorf("QRDataURL(\"\") = %q, %v; want empty, nil", got, err)
	}
	got, err := QRDataURL("2@some-payload")
	if err != nil {
		t.Fatalf("QRDataURL() error = %v", err)
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("QRDataURL() = %q, want %s prefix", got, prefix)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got, prefix))
	if err != nil {
		t.Fatalf("payload is not valid base64: %v", err)
	}
	// PNG magic number.
	if len(raw) < 8 || string(raw[1:4]) != "PNG" {
		t.Errorf("decoded bytes are not a PNG (magic = %x)", raw[:min(8, len(raw))])
	}
}

// TestProvisionPopulatesCertFields verifies the provision result carries the
// gateway cert PEM + plaintext cert refresh URL from the injected deps (the
// node half of the cert-publish contract, aceteam-ai/citadel-cli#448).
func TestProvisionPopulatesCertFields(t *testing.T) {
	bridge := &fakeBridge{health: &Health{}, qr: "x"}
	deps, _ := baseDeps(t, bridge)
	deps.GatewayCertPEM = func() string { return "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n" }
	deps.CertRefreshURL = func() string { return "http://100.64.0.7:8080/gateway-cert.pem" }

	res, err := Provision(context.Background(), ProvisionRequest{}, deps)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if res.GatewayCertPEM != "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n" {
		t.Errorf("GatewayCertPEM = %q, want the injected PEM", res.GatewayCertPEM)
	}
	if res.CertRefreshURL != "http://100.64.0.7:8080/gateway-cert.pem" {
		t.Errorf("CertRefreshURL = %q, want the injected refresh URL", res.CertRefreshURL)
	}
}

// TestProvisionCertFieldsEmptyOffMeshOrNoTLS verifies that when the cert deps
// report no cert / off-mesh (empty strings) or are nil, the result fields stay
// empty (older backends ignore them; omitempty on the wire keys).
func TestProvisionCertFieldsEmptyOffMeshOrNoTLS(t *testing.T) {
	// Case 1: deps present but return "" (gateway --gateway-no-tls / off-mesh).
	bridge := &fakeBridge{health: &Health{}, qr: "x"}
	deps, _ := baseDeps(t, bridge)
	deps.GatewayCertPEM = func() string { return "" }
	deps.CertRefreshURL = func() string { return "" }
	res, err := Provision(context.Background(), ProvisionRequest{}, deps)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if res.GatewayCertPEM != "" || res.CertRefreshURL != "" {
		t.Errorf("cert fields should be empty when deps return empty, got pem=%q url=%q", res.GatewayCertPEM, res.CertRefreshURL)
	}

	// Case 2: deps entirely absent (nil) -> fields remain empty, no panic.
	bridge2 := &fakeBridge{health: &Health{}, qr: "x"}
	deps2, _ := baseDeps(t, bridge2)
	res2, err := Provision(context.Background(), ProvisionRequest{}, deps2)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if res2.GatewayCertPEM != "" || res2.CertRefreshURL != "" {
		t.Errorf("cert fields should be empty when deps are nil, got pem=%q url=%q", res2.GatewayCertPEM, res2.CertRefreshURL)
	}
}

// --- image upgrade legibility (aceteam-ai/citadel-cli#718) -------------------

// scriptedImageID returns a BridgeImageID stub that yields the given values in
// order (the first call is the pre-deploy sample, the second the post-deploy
// one) and records how many times it was called.
func scriptedImageID(values ...string) (func() string, *int) {
	calls := 0
	return func() string {
		v := ""
		if calls < len(values) {
			v = values[calls]
		}
		calls++
		return v
	}, &calls
}

// TestProvisionImageUpgradeDetection pins the upgraded-vs-unchanged contract:
// `already_linked` must stop being indistinguishable from a real upgrade. An
// UNKNOWN image (bridge not previously running, Docker unreadable, dep absent)
// must never be reported as an upgrade.
func TestProvisionImageUpgradeDetection(t *testing.T) {
	tests := []struct {
		name         string
		images       []string // successive BridgeImageID() results; nil = no dep
		nilDep       bool
		wantUpgraded bool
		wantBefore   string
		wantAfter    string
	}{
		{
			name:         "recreated onto a new image",
			images:       []string{"sha256:af88f094", "sha256:8fc94272"},
			wantUpgraded: true,
			wantBefore:   "sha256:af88f094",
			wantAfter:    "sha256:8fc94272",
		},
		{
			name:         "no-op re-provision on the same image",
			images:       []string{"sha256:af88f094", "sha256:af88f094"},
			wantUpgraded: false,
			wantBefore:   "sha256:af88f094",
			wantAfter:    "sha256:af88f094",
		},
		{
			name:         "first deploy has no before image",
			images:       []string{"", "sha256:8fc94272"},
			wantUpgraded: false,
			wantBefore:   "",
			wantAfter:    "sha256:8fc94272",
		},
		{
			name:         "unreadable after image is unknown, not an upgrade",
			images:       []string{"sha256:af88f094", ""},
			wantUpgraded: false,
			wantBefore:   "sha256:af88f094",
			wantAfter:    "",
		},
		{
			name:         "nil dep degrades to unknown",
			nilDep:       true,
			wantUpgraded: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bridge := &fakeBridge{health: &Health{LoggedIn: true}}
			deps, _ := baseDeps(t, bridge)
			var calls *int
			if !tt.nilDep {
				deps.BridgeImageID, calls = scriptedImageID(tt.images...)
			}

			res, err := Provision(context.Background(), ProvisionRequest{}, deps)
			if err != nil {
				t.Fatalf("Provision() error = %v", err)
			}
			if res.Upgraded != tt.wantUpgraded {
				t.Errorf("Upgraded = %v, want %v", res.Upgraded, tt.wantUpgraded)
			}
			if res.ImageIDBefore != tt.wantBefore {
				t.Errorf("ImageIDBefore = %q, want %q", res.ImageIDBefore, tt.wantBefore)
			}
			if res.ImageIDAfter != tt.wantAfter {
				t.Errorf("ImageIDAfter = %q, want %q", res.ImageIDAfter, tt.wantAfter)
			}
			// The already-linked path must still carry the image fields: that
			// combination (already_linked + unchanged image) IS the false-green.
			if !res.AlreadyLinked {
				t.Error("AlreadyLinked = false, want true for this fixture")
			}
			if calls != nil && *calls != 2 {
				t.Errorf("BridgeImageID called %d times, want exactly 2 (before + after deploy)", *calls)
			}
		})
	}
}

// TestProvisionSamplesImageAroundDeploy pins the ORDER: the "before" sample must
// be taken before DeployCompose runs and the "after" sample after it returns.
// Sampling both afterwards would always report "unchanged" and re-hide the bug.
func TestProvisionSamplesImageAroundDeploy(t *testing.T) {
	bridge := &fakeBridge{health: &Health{LoggedIn: true}}
	deps, _ := baseDeps(t, bridge)

	var order []string
	deps.DeployCompose = func(servicesDir string, env map[string]string) error {
		order = append(order, "deploy")
		return nil
	}
	deps.BridgeImageID = func() string {
		order = append(order, "image")
		return ""
	}

	if _, err := Provision(context.Background(), ProvisionRequest{}, deps); err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	want := []string{"image", "deploy", "image"}
	if len(order) != len(want) {
		t.Fatalf("call order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("call order = %v, want %v", order, want)
		}
	}
}

// TestProvisionSurfacesImagePullError verifies a failed image pull does NOT fail
// the provision (the cached image still serves) but IS reported, so a caller can
// never read "provisioned" as "running the latest image".
func TestProvisionSurfacesImagePullError(t *testing.T) {
	bridge := &fakeBridge{health: &Health{LoggedIn: true}}
	deps, deployed := baseDeps(t, bridge)
	deps.BridgeImageID = func() string { return "sha256:af88f094" }
	deps.ImagePullError = func() string { return "docker compose pull failed: unauthorized" }

	res, err := Provision(context.Background(), ProvisionRequest{}, deps)
	if err != nil {
		t.Fatalf("Provision() error = %v, want the provision to survive a pull failure", err)
	}
	if !*deployed {
		t.Error("expected DeployCompose to still run after a pull failure")
	}
	if res.ImagePullError != "docker compose pull failed: unauthorized" {
		t.Errorf("ImagePullError = %q, want the injected pull error", res.ImagePullError)
	}
	if res.Upgraded {
		t.Error("Upgraded = true, want false: a failed pull cannot have upgraded anything")
	}
}

// TestProvisionNoPullErrorWhenPullSucceeded is the negative half: an empty
// ImagePullError must not be turned into a spurious warning field.
func TestProvisionNoPullErrorWhenPullSucceeded(t *testing.T) {
	bridge := &fakeBridge{health: &Health{LoggedIn: true}}
	deps, _ := baseDeps(t, bridge)
	deps.ImagePullError = func() string { return "" }

	res, err := Provision(context.Background(), ProvisionRequest{}, deps)
	if err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if res.ImagePullError != "" {
		t.Errorf("ImagePullError = %q, want empty", res.ImagePullError)
	}
}
