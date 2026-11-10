package cmd

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
)

func enrollmentIdentityFixture(t *testing.T) *nodeidentity.Store {
	t.Helper()
	root := t.TempDir()
	store := nodeidentity.New(filepath.Join(root, "identity"))
	savedStore, savedDir, savedFix, savedFlow := loginIdentityStore, nodeConfigDirFn, fixStatePermissionsFn, deviceAuthorizationWithCSR
	t.Cleanup(func() {
		loginIdentityStore, nodeConfigDirFn, fixStatePermissionsFn, deviceAuthorizationWithCSR = savedStore, savedDir, savedFix, savedFlow
	})
	loginIdentityStore = func() *nodeidentity.Store { return store }
	nodeConfigDirFn = func() string { return root }
	fixStatePermissionsFn = func() {}
	return store
}

func assertEnrollmentCSRMatchesSigner(t *testing.T, store *nodeidentity.Store, csrPEM string) {
	t.Helper()
	block, rest := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatal("enrollment did not send a PEM CSR")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		t.Fatal("enrollment CSR is not self-signed")
	}
	public, err := store.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil || !bytes.Equal(der, csr.RawSubjectPublicKeyInfo) {
		t.Fatal("enrollment CSR differs from the receipt signer SPKI")
	}
}

func TestSharedEnrollmentCarriesSignerCSRToActualTokenEndpoint(t *testing.T) {
	t.Setenv(deviceCertificateEnrollmentEnv, "true")
	store := enrollmentIdentityFixture(t)
	mock := nexus.StartMockDeviceAuthServer(1)
	defer mock.Close()
	result, err := runDeviceAuthFlow(mock.URL(), false)
	if err != nil || result == nil || result.Token == nil || result.Token.Authkey == "" {
		t.Fatal("legacy server no longer enrolls")
	}
	requests := mock.TokenRequestCSRs()
	if len(requests) == 0 {
		t.Fatal("no token request observed")
	}
	for _, csr := range requests {
		assertEnrollmentCSRMatchesSigner(t, store, csr)
	}
	if store.HasLeaf() || deviceEnrollmentHostname(result, "display-name") != "display-name" {
		t.Fatal("ignoring server manufactured an enrolled serving identity")
	}
}

func TestSharedEnrollmentValidatesAdditiveIdentityAndPreservesCredentials(t *testing.T) {
	t.Setenv(deviceCertificateEnrollmentEnv, "true")
	for _, name := range []string{"issued", "legacy", "bad-leaf", "partial", "unexpected-uid", "auth-failure"} {
		t.Run(name, func(t *testing.T) {
			store := enrollmentIdentityFixture(t)
			key, err := store.GetOrCreateKey()
			if err != nil {
				t.Fatal(err)
			}
			leaf, chain := testLoginChain(t, key, 101)
			token := &nexus.TokenResponse{Authkey: "fixture-authkey", DeviceAPIToken: "fixture-bearer", OrgID: "fixture-org", NodeUID: "uid-abc", LeafPem: leaf, ChainPem: chain}
			switch name {
			case "legacy":
				token.NodeUID, token.LeafPem, token.ChainPem = "", "", ""
			case "bad-leaf":
				token.LeafPem, token.ChainPem = testLoginChain(t, newKey(t), 102)
			case "partial":
				token.ChainPem = ""
			case "unexpected-uid":
				token.NodeUID = "different-node"
			}
			expectedErr := errors.New("fixture approval denied")
			deviceAuthorizationWithCSR = func(origin string, force bool, csr string) (*DeviceAuthResult, error) {
				if origin != "https://fixture.invalid" || !force {
					t.Fatal("enrollment invocation changed")
				}
				assertEnrollmentCSRMatchesSigner(t, store, csr)
				if name == "auth-failure" {
					return nil, expectedErr
				}
				return &DeviceAuthResult{Token: token, DeviceCode: "fixture-grant"}, nil
			}
			result, err := runDeviceAuthFlow("https://fixture.invalid", true)
			if name == "auth-failure" {
				if err != expectedErr || result != nil || store.HasLeaf() {
					t.Fatal("failed authorization accepted an identity")
				}
				return
			}
			if err != nil || result == nil || result.Token.Authkey != "fixture-authkey" || result.Token.DeviceAPIToken != "fixture-bearer" {
				t.Fatal("additive certificate failure broke enrollment")
			}
			if result.Token == token {
				t.Fatal("accepted enrollment aliases producer token")
			}
			if name == "issued" {
				if !store.HasLeaf() || deviceEnrollmentHostname(result, "display-name") != "display-name" || loadLoginNodeUID() != "" {
					t.Fatal("validated identity was not persisted and selected")
				}
			} else if store.HasLeaf() || deviceEnrollmentHostname(result, "display-name") != "display-name" || result.identityNodeUID != "" {
				t.Fatal("unverified bundle selected a serving identity")
			}
		})
	}
}

func TestSharedEnrollmentCSRFailureIsNonfatalAndNeverTrustsUnsolicitedLeaf(t *testing.T) {
	t.Setenv(deviceCertificateEnrollmentEnv, "true")
	store := enrollmentIdentityFixture(t)
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	loginIdentityStore = func() *nodeidentity.Store { return nodeidentity.New(filepath.Join(parent, "identity")) }
	deviceAuthorizationWithCSR = func(_ string, _ bool, csr string) (*DeviceAuthResult, error) {
		if csr != "" {
			t.Fatal("failed CSR was submitted")
		}
		leaf, chain := testLoginChain(t, newKey(t), 103)
		return &DeviceAuthResult{Token: &nexus.TokenResponse{Authkey: "fixture-authkey", NodeUID: "uid-abc", LeafPem: leaf, ChainPem: chain}}, nil
	}
	result, err := runDeviceAuthFlow("https://fixture.invalid", false)
	if err != nil || result == nil || result.Token.Authkey != "fixture-authkey" || result.identityNodeUID != "" || store.HasLeaf() {
		t.Fatal("CSR failure failed auth or accepted an unsolicited identity")
	}
}

func TestControlCenterEnrollmentBindsCSRAndDefersLeafUntilAccepted(t *testing.T) {
	t.Setenv(deviceCertificateEnrollmentEnv, "true")
	store := enrollmentIdentityFixture(t)
	key, err := store.GetOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	leaf, chain := testLoginChain(t, key, 104)
	calls, connected := 0, 0
	config, err := ccStartDeviceAuthIdentityFlowWith(func() (*nexus.DeviceCodeResponse, error) {
		return &nexus.DeviceCodeResponse{DeviceCode: "grant-a", UserCode: "fixture", ExpiresIn: 60, Interval: 5}, nil
	}, time.Now,
		func(code, csr string) (*nexus.TokenResponse, error) {
			calls++
			if code != "grant-a" {
				t.Fatal("grant rebound")
			}
			assertEnrollmentCSRMatchesSigner(t, store, csr)
			return &nexus.TokenResponse{Authkey: "fixture-authkey", LeafPem: leaf, ChainPem: chain, NodeUID: "uid-abc"}, nil
		},
		func(token *nexus.TokenResponse, code, uid string) error {
			connected++
			if token.Authkey != "fixture-authkey" || code != "grant-a" || uid != "uid-abc" {
				t.Fatal("acceptance lost current identity")
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	token, err := config.PollToken("grant-a", 5)
	if err != nil || token == nil || calls != 1 || store.HasLeaf() {
		t.Fatal("poll persisted identity before admission")
	}
	if err := config.Connect(token, "grant-b"); err == nil || store.HasLeaf() || connected != 0 {
		t.Fatal("different grant accepted certificate")
	}
	if _, err := config.PollToken("grant-b", 5); err == nil || calls != 1 {
		t.Fatal("bound poller fetched another grant")
	}
	if err := config.Connect(token, "grant-a"); err != nil || connected != 1 || !store.HasLeaf() {
		t.Fatal("accepted grant lost its certificate", err)
	}
}

func TestCertificateDiagnosticsRenderWithoutClaimingRemoteVerification(t *testing.T) {
	store := enrollmentIdentityFixture(t)
	key, err := store.GetOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	leaf, chain := testLoginChain(t, key, 105)
	if err := store.StoreLeaf(leaf, chain); err != nil {
		t.Fatal(err)
	}
	status := readNodeCertificateStatus()
	if status.SPKIMatchesSigner == nil || !*status.SPKIMatchesSigner {
		t.Fatal("diagnostic did not inspect canonical signing key")
	}
	var whoami, doctor bytes.Buffer
	renderIdentity(&whoami, NodeIdentity{Certificate: &status})
	renderDoctorReport(&doctor, doctorReport{certificate: &status})
	for _, text := range []string{whoami.String(), doctor.String()} {
		if !strings.Contains(text, "SPKI matches signing key") || !strings.Contains(text, "CA trust and revocation were not checked") {
			t.Fatal("diagnostic omitted comparison or overstated verification")
		}
		if strings.Contains(text, store.Dir()) || strings.Contains(text, "PRIVATE KEY") {
			t.Fatal("diagnostic exposed storage details")
		}
	}
}

func TestSharedEnrollmentKeepsKnownGoodLeafOnBadBundle(t *testing.T) {
	t.Setenv(deviceCertificateEnrollmentEnv, "true")
	store := enrollmentIdentityFixture(t)
	key, err := store.GetOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	leaf, chain := testLoginChain(t, key, 106)
	if err := store.StoreLeaf(leaf, chain); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.LeafPath())
	if err != nil {
		t.Fatal(err)
	}
	badLeaf, badChain := testLoginChain(t, newKey(t), 107)
	deviceAuthorizationWithCSR = func(_ string, _ bool, csr string) (*DeviceAuthResult, error) {
		assertEnrollmentCSRMatchesSigner(t, store, csr)
		return &DeviceAuthResult{Token: &nexus.TokenResponse{Authkey: "fixture-authkey", LeafPem: badLeaf, ChainPem: badChain, NodeUID: "uid-abc"}}, nil
	}
	result, err := runDeviceAuthFlow("https://fixture.invalid", false)
	if err != nil || result == nil || result.Token.Authkey != "fixture-authkey" || result.identityNodeUID != "" {
		t.Fatal("bad additive bundle broke authorization or selected a serving identity")
	}
	after, err := os.ReadFile(store.LeafPath())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("bad bundle overwrote known-good certificate")
	}
}

func TestDeviceEnrollmentHostnameAlwaysKeepsDisplayName(t *testing.T) {
	raw := &DeviceAuthResult{Token: &nexus.TokenResponse{NodeUID: "unvalidated-node"}}
	if deviceEnrollmentHostname(raw, "friendly") != "friendly" || deviceEnrollmentHostname(nil, "friendly") != "friendly" {
		t.Fatal("raw server UID replaced display/serving name")
	}
	raw.identityNodeUID = "uid-abc"
	if deviceEnrollmentHostname(raw, "friendly") != "friendly" {
		t.Fatal("validated certificate UID changed the mesh hostname")
	}
}

func TestPostProvisionDeviceEnrollmentPreservesIdentityAndCurrentGrant(t *testing.T) {
	token := &nexus.TokenResponse{Authkey: "fixture-authkey", NodeUID: "unvalidated-response"}
	result := &DeviceAuthResult{Token: token, DeviceCode: "current-grant", identityNodeUID: "uid-abc"}
	var steps []string
	err := connectDeviceEnrollmentAsWith(result, "friendly", func(display, serving, authkey string) error {
		steps = append(steps, "connect")
		if display != "friendly" || serving != "friendly" || authkey != "fixture-authkey" {
			t.Fatal("post-provisioning registration changed the display name or grant")
		}
		// Link must use the captured grant, even if its caller changes state.
		result.DeviceCode = "another-grant"
		token.Authkey = "replacement-key"
		return nil
	}, func(current *DeviceAuthResult) error {
		steps = append(steps, "link")
		if current.DeviceCode != "current-grant" || current.Token.Authkey != "fixture-authkey" {
			t.Fatal("post-provisioning link used another grant")
		}
		return nil
	}, func() error {
		t.Fatal("successful link disconnected")
		return nil
	})
	if err != nil || strings.Join(steps, ",") != "connect,link" {
		t.Fatal("device grant was not connected and linked in order", err)
	}
}

func TestPostProvisionDeviceEnrollmentFailsBeforeEffectsWithoutCurrentGrant(t *testing.T) {
	for _, result := range []*DeviceAuthResult{nil, {}, {Token: &nexus.TokenResponse{Authkey: "fixture"}}, {DeviceCode: "grant", Token: &nexus.TokenResponse{}}} {
		err := connectDeviceEnrollmentAsWith(result, "friendly", func(string, string, string) error {
			t.Fatal("invalid grant connected")
			return nil
		}, func(*DeviceAuthResult) error {
			t.Fatal("invalid grant linked")
			return nil
		}, func() error {
			t.Fatal("invalid grant disconnected")
			return nil
		})
		if err == nil {
			t.Fatal("missing grant accepted")
		}
	}
}

func TestPostProvisionDeviceEnrollmentDisconnectsOnRefusedLink(t *testing.T) {
	result := &DeviceAuthResult{DeviceCode: "grant", Token: &nexus.TokenResponse{Authkey: "fixture"}}
	want := errors.New("fixture link refusal")
	disconnected := false
	err := connectDeviceEnrollmentAsWith(result, "friendly", func(display, serving, _ string) error {
		if display != "friendly" || serving != "friendly" {
			t.Fatal("leafless enrollment changed legacy serving name")
		}
		return nil
	}, func(*DeviceAuthResult) error { return want }, func() error { disconnected = true; return nil })
	if !errors.Is(err, want) || !disconnected {
		t.Fatal("refused link was reported as successful or left connected")
	}
}

func TestDisabledEnrollmentCertificatesNeverOpenIdentityState(t *testing.T) {
	for _, value := range []string{"", "false", "0", "not-an-opt-in"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(deviceCertificateEnrollmentEnv, value)
			store := enrollmentIdentityFixture(t)
			reads := 0
			loginIdentityStore = func() *nodeidentity.Store { reads++; return store }
			unsolicited := &nexus.TokenResponse{Authkey: "fixture-authkey", DeviceAPIToken: "fixture-device", NodeUID: "unsolicited-uid", LeafPem: "unsolicited-leaf", ChainPem: "unsolicited-chain"}
			deviceAuthorizationWithCSR = func(_ string, _ bool, csr string) (*DeviceAuthResult, error) {
				if csr != "" {
					t.Fatal("disabled enrollment submitted a CSR")
				}
				return &DeviceAuthResult{Token: unsolicited, DeviceCode: "fixture-grant"}, nil
			}
			result, err := runDeviceAuthFlow("https://fixture.invalid", false)
			if err != nil || result == nil || result.Token.Authkey != unsolicited.Authkey || result.Token.DeviceAPIToken != unsolicited.DeviceAPIToken || result.identityNodeUID != "" {
				t.Fatal("default enrollment changed its grant or accepted an unsolicited certificate")
			}
			connected := false
			config, err := ccStartDeviceAuthIdentityFlowWith(func() (*nexus.DeviceCodeResponse, error) {
				return &nexus.DeviceCodeResponse{DeviceCode: "fixture-grant", ExpiresIn: 60, Interval: 1}, nil
			}, time.Now,
				func(grant, csr string) (*nexus.TokenResponse, error) {
					if grant != "fixture-grant" || csr != "" {
						t.Fatal("disabled Control Center changed its grant or submitted a CSR")
					}
					return unsolicited, nil
				},
				func(token *nexus.TokenResponse, grant, uid string) error {
					connected = true
					if token.Authkey != unsolicited.Authkey || grant != "fixture-grant" || uid != "" {
						t.Fatal("disabled Control Center changed grant or persisted identity")
					}
					return nil
				})
			if err != nil {
				t.Fatal(err)
			}
			token, err := config.PollToken("fixture-grant", 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := config.Connect(token, "fixture-grant"); err != nil {
				t.Fatal(err)
			}
			if !connected || reads != 0 {
				t.Fatalf("disabled certificate path touched identity state: connected=%v reads=%d", connected, reads)
			}
			entries, err := os.ReadDir(nodeConfigDirFn())
			if err != nil || len(entries) != 0 {
				t.Fatal("disabled certificate path created local identity/config files", err)
			}
		})
	}
}

func TestDefaultEnrollmentOmitsCSRAtActualTokenEndpoint(t *testing.T) {
	t.Setenv(deviceCertificateEnrollmentEnv, "")
	enrollmentIdentityFixture(t)
	mock := nexus.StartMockDeviceAuthServer(1)
	defer mock.Close()
	result, err := runDeviceAuthFlow(mock.URL(), false)
	if err != nil || result == nil || result.Token == nil {
		t.Fatal("default enrollment no longer completes", err)
	}
	csrs := mock.TokenRequestCSRs()
	if len(csrs) == 0 {
		t.Fatal("no token poll observed")
	}
	for _, csr := range csrs {
		if csr != "" {
			t.Fatal("ordinary enrollment sent csr_pem without an opt-in")
		}
	}
}

func TestSharedEnrollmentCertificateDoesNotSetWorkerServingName(t *testing.T) {
	t.Setenv(deviceCertificateEnrollmentEnv, "true")
	store := enrollmentIdentityFixture(t)
	key, err := store.GetOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	leaf, chain := testLoginChain(t, key, 108)
	oldName := workNodeName
	workNodeName = "friendly"
	t.Cleanup(func() { workNodeName = oldName })
	deviceAuthorizationWithCSR = func(_ string, _ bool, csr string) (*DeviceAuthResult, error) {
		assertEnrollmentCSRMatchesSigner(t, store, csr)
		return &DeviceAuthResult{Token: &nexus.TokenResponse{Authkey: "fixture-authkey", NodeUID: "uid-abc", LeafPem: leaf, ChainPem: chain}}, nil
	}
	result, err := runDeviceAuthFlow("https://fixture.invalid", false)
	if err != nil || result == nil || result.identityNodeUID != "uid-abc" || !store.HasLeaf() {
		t.Fatal("opted-in enrollment lost its validated certificate", err)
	}
	if loadLoginNodeUID() != "" || getWorkHostname() != "friendly" {
		t.Fatal("certificate enrollment changed a later worker reconnect hostname")
	}
	data, err := os.ReadFile(filepath.Join(nodeConfigDirFn(), "config.yaml"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("login_node_uid")) {
		t.Fatal("certificate UID leaked into serving-name configuration")
	}
}

func TestDeviceEnrollmentRetiresStaleLoginServingName(t *testing.T) {
	for _, mode := range []string{"init-or-enroll", "control-center", "refused-link"} {
		t.Run(mode, func(t *testing.T) {
			store := enrollmentIdentityFixture(t)
			key, err := store.GetOrCreateKey()
			if err != nil {
				t.Fatal(err)
			}
			leaf, chain := testLoginChain(t, key, 109)
			if _, err := persistLoginIdentityBundle(store, &key.PublicKey, leaf, chain, "uid-abc"); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(nodeConfigDirFn(), "config.yaml")
			if err := os.WriteFile(configPath, []byte("hostname: display-name\ndevice_api_token: preserved\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := saveLoginNodeUID("uid-abc"); err != nil {
				t.Fatal(err)
			}
			oldName := workNodeName
			workNodeName = "display-name"
			t.Cleanup(func() { workNodeName = oldName })
			if got := getWorkHostname(); got != "node-uid-abc" {
				t.Fatalf("stale worker hostname before device transition = %q", got)
			}

			linkErr := errors.New("fixture link refused")
			linked := false
			link := func(result *DeviceAuthResult) error {
				linked = true
				if result == nil || result.DeviceCode != "current-grant" || result.Token.Authkey != "fixture-key" {
					t.Fatal("device transition did not retain its current grant")
				}
				if mode == "refused-link" {
					return linkErr
				}
				return nil
			}
			finish := func(result *DeviceAuthResult) error {
				return finishDeviceEnrollmentLink(result, link)
			}
			var transitionErr error
			if mode == "init-or-enroll" {
				transitionErr = finish(&DeviceAuthResult{DeviceCode: "current-grant", Token: &nexus.TokenResponse{Authkey: "fixture-key"}})
			} else {
				transitionErr = ccConnectDeviceEnrollmentWith(&nexus.TokenResponse{Authkey: "fixture-key"}, "current-grant", func() error { return nil }, finish, func() error { return nil })
			}
			if !linked {
				t.Fatal("device transition did not link its current grant")
			}
			if mode == "refused-link" {
				if !errors.Is(transitionErr, linkErr) || getWorkHostname() != "node-uid-abc" {
					t.Fatal("refused device link retired the prior login identity")
				}
				return
			}
			if transitionErr != nil {
				t.Fatal(transitionErr)
			}
			if got := getWorkHostname(); got != "display-name" {
				t.Fatalf("worker reconnected under %q after device transition", got)
			}
			data, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte("login_node_uid")) || !bytes.Contains(data, []byte("device_api_token: preserved")) {
				t.Fatal("device transition did not reconcile the login UID without altering credentials")
			}
		})
	}
}
