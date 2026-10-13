package services

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"gopkg.in/yaml.v3"
)

//go:embed radiology-service/service.yaml
var radiologyServiceYAML []byte

//go:embed radiology-service/compose.yml
var radiologyComposeYAML []byte

func TestRadiologyServiceManifest(t *testing.T) {
	var m catalog.ServiceManifest
	if err := yaml.Unmarshal(radiologyServiceYAML, &m); err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != catalog.CurrentSchemaVersion || m.Name != "radiology" || m.Category != "tools" {
		t.Fatalf("unexpected module identity: schema=%d name=%q category=%q", m.SchemaVersion, m.Name, m.Category)
	}
	if m.Requires.GPU || strings.Join(m.Requires.Arch, ",") != "amd64,arm64" {
		t.Errorf("radiology requires CPU only on the two verified image architectures: %+v", m.Requires)
	}
	if m.Gateway != nil || m.Sandbox.HostNetwork || len(m.Sandbox.Devices) != 0 {
		t.Error("radiology must not declare automatic exposure, host networking or devices")
	}
	if strings.Join(m.NodeTags, ",") != "radiology" {
		t.Errorf("node_tags = %v", m.NodeTags)
	}
	if !catalog.HasHealthProbe(m.HealthCheck) || m.HealthCheck.Endpoint != "/system" || m.HealthCheck.Port != RadiologyHostPort {
		t.Errorf("transport-liveness health check must address the published /system: %+v", m.HealthCheck)
	}
	wantPorts := map[int]int{RadiologyHostPort: 8042, RadiologyDICOMPort: 4242}
	if len(m.Ports) != len(wantPorts) {
		t.Fatalf("ports = %v", m.Ports)
	}
	for _, p := range m.Ports {
		if wantPorts[p.Host] != p.Container || p.Protocol != "tcp" {
			t.Errorf("unexpected mapping %+v", p)
		}
		delete(wantPorts, p.Host)
	}
	if len(wantPorts) != 0 {
		t.Errorf("missing ports %v", wantPorts)
	}
	cfg := map[string]catalog.ConfigVar{}
	for _, c := range m.Config {
		cfg[c.Name] = c
	}
	secret := cfg["RADIOLOGY_HTTP_PASSWORD"]
	if !secret.Required || secret.Generate != catalog.GenerateSecret || secret.Default != "" {
		t.Errorf("HTTP secret must be required, generated and without a shared default: %+v", secret)
	}
	for _, name := range []string{"PACS_AET", "PACS_HOST"} {
		if c := cfg[name]; !c.Required || c.Default != "" {
			t.Errorf("%s must identify an explicitly supplied peer", name)
		}
	}
	for name, want := range map[string]string{"RADIOLOGY_AET": "CITADEL", "PACS_PORT": "4242", "RADIOLOGY_DICOM_PORT": "4242", "RADIOLOGY_STABLE_AGE": "60"} {
		if got := cfg[name].Default; got != want {
			t.Errorf("%s default = %q, want %q", name, got, want)
		}
	}
	if _, exists := cfg["RADIOLOGY_VIEWER"]; exists {
		t.Error("C1 must not offer an enable toggle before authenticated exposure and banner implementation")
	}
	if len(m.Volumes) != 1 || m.Volumes[0].Host != "~/.citadel-cli/radiology/orthanc" || m.Volumes[0].Container != "/var/lib/orthanc" {
		t.Errorf("raw DICOM/index must stay outside the workspace: %v", m.Volumes)
	}
}

func TestRadiologyComposeHardening(t *testing.T) {
	var compose struct {
		Services map[string]struct {
			Image         string            `yaml:"image"`
			ContainerName string            `yaml:"container_name"`
			NetworkMode   string            `yaml:"network_mode"`
			Privileged    bool              `yaml:"privileged"`
			Ports         []string          `yaml:"ports"`
			Volumes       []string          `yaml:"volumes"`
			Environment   map[string]string `yaml:"environment"`
			Healthcheck   struct {
				Test    []string `yaml:"test"`
				Timeout string   `yaml:"timeout"`
			} `yaml:"healthcheck"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(radiologyComposeYAML, &compose); err != nil {
		t.Fatal(err)
	}
	if len(compose.Services) != 1 {
		t.Fatal("C1 must contain only Orthanc, not a bridge or init service")
	}
	svc, ok := compose.Services["orthanc"]
	if !ok {
		t.Fatal("missing orthanc service")
	}
	const image = "docker.io/orthancteam/orthanc@sha256:d2705f2c56547e55ce9bc2250fd22d05d326493b3f3d9fa191ecb7db8ca6b5a2"
	if svc.Image != image || svc.ContainerName != "citadel-radiology" || svc.Privileged || svc.NetworkMode != "" {
		t.Errorf("unexpected image/container/privilege/network configuration: %q %q privileged=%v network=%q", svc.Image, svc.ContainerName, svc.Privileged, svc.NetworkMode)
	}
	wantPorts := []string{"127.0.0.1:${CITADEL_RADIOLOGY_HOST_PORT:-8216}:8042", "${RADIOLOGY_DICOM_PORT:-4242}:4242"}
	if strings.Join(svc.Ports, ",") != strings.Join(wantPorts, ",") {
		t.Errorf("only the configured DICOM port may be LAN-facing: %v", svc.Ports)
	}
	if len(svc.Volumes) != 1 || svc.Volumes[0] != "${HOME:?node home required}/.citadel-cli/radiology/orthanc:/var/lib/orthanc" {
		t.Errorf("unexpected raw-data bind; no workspace or authorized roots: %v", svc.Volumes)
	}
	wantEnv := map[string]string{
		"ORTHANC__DICOM_CHECK_CALLED_AET": "true", "ORTHANC__DICOM_CHECK_MODALITY_HOST": "true",
		"ORTHANC__DICOM_ALWAYS_ALLOW_STORE": "false", "ORTHANC__DICOM_ALWAYS_ALLOW_ECHO": "false",
		"ORTHANC__DICOM_ALWAYS_ALLOW_FIND": "false", "ORTHANC__DICOM_ALWAYS_ALLOW_FIND_WORKLIST": "false",
		"ORTHANC__DICOM_ALWAYS_ALLOW_GET": "false", "ORTHANC__DICOM_ALWAYS_ALLOW_MOVE": "false",
		"ORTHANC__DICOM_MODALITIES_IN_DATABASE": "false", "ORTHANC__AUTHENTICATION_ENABLED": "true",
		"ORTHANC_PASSWORD":               "${RADIOLOGY_HTTP_PASSWORD:?generated HTTP secret required}",
		"ORTHANC__REMOTE_ACCESS_ALLOWED": "true", "ORTHANC__STORAGE_DIRECTORY": "/var/lib/orthanc/storage",
		"ORTHANC__INDEX_DIRECTORY": "/var/lib/orthanc/index", "ORTHANC__STABLE_AGE": "${RADIOLOGY_STABLE_AGE:-60}",
		"ORTHANC__DICOM_AET":                          "${RADIOLOGY_AET:-CITADEL}",
		"ORTHANC__DICOM_MODALITIES__PACS__A_E_T":      "${PACS_AET:?allowed peer AE required}",
		"ORTHANC__DICOM_MODALITIES__PACS__HOST":       "${PACS_HOST:?allowed peer host required}",
		"ORTHANC__DICOM_MODALITIES__PACS__PORT":       "${PACS_PORT:-4242}",
		"ORTHANC__DICOM_MODALITIES__PACS__ALLOW_ECHO": "true", "ORTHANC__DICOM_MODALITIES__PACS__ALLOW_STORE": "true",
		"ORTHANC__DICOM_MODALITIES__PACS__ALLOW_FIND": "false", "ORTHANC__DICOM_MODALITIES__PACS__ALLOW_FIND_WORKLIST": "false",
		"ORTHANC__DICOM_MODALITIES__PACS__ALLOW_GET": "false", "ORTHANC__DICOM_MODALITIES__PACS__ALLOW_MOVE": "false",
		"ORTHANC__DICOM_MODALITIES__PACS__ALLOW_STORAGE_COMMITMENT": "false",
		"DICOM_WEB_PLUGIN_ENABLED":                                  "true", "OHIF_PLUGIN_ENABLED": "false", "ORTHANC_EXPLORER_2_ENABLED": "false",
		"VERBOSE_STARTUP": "false", "VERBOSE_ENABLED": "false", "TRACE_ENABLED": "false",
		"ORTHANC__NAME": "Citadel Radiology - NOT FOR DIAGNOSTIC USE",
	}
	if len(svc.Environment) != len(wantEnv) {
		t.Errorf("unexpected extra environment settings: got %d, want %d", len(svc.Environment), len(wantEnv))
	}
	for name, want := range wantEnv {
		if got := svc.Environment[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	probe := svc.Healthcheck.Test
	if len(probe) != 7 || strings.Join(probe[:6], ",") != "CMD,python3,-I,-S,-B,-c" || svc.Healthcheck.Timeout != "10s" {
		t.Fatalf("unexpected bounded health probe argv: %v", probe)
	}
	for _, contract := range []string{`os.environ["ORTHANC_PASSWORD"]`, `"http://127.0.0.1:8042/system"`, `ProxyHandler({})`, `NoRedirect()`, `timeout=5`, `response.status != 200`, `response.read(65537)`, `len(body) > 65536`, `except Exception:`, `sys.exit(1)`} {
		if !strings.Contains(probe[6], contract) {
			t.Errorf("health probe lost contract %q", contract)
		}
	}
	for _, leak := range []string{"print(", "traceback", "--pwd", "HTTPError as", "${RADIOLOGY_HTTP_PASSWORD}"} {
		if strings.Contains(probe[6], leak) {
			t.Errorf("health probe must not expose credential/body/error via %q", leak)
		}
	}
}

func TestRadiologyHostPortsRegistered(t *testing.T) {
	if ServiceHostPorts["radiology"] != RadiologyHostPort || serviceHostPortEnv["radiology"] != EnvRadiologyHostPort {
		t.Fatal("Orthanc HTTP port must be registered for compose injection")
	}
	if RadiologyHostPort != 8216 || RadiologyDICOMPort != 4242 || RadiologyHostPort == RadiologyDICOMPort {
		t.Fatal("radiology port defaults drifted")
	}
	for name, port := range ServiceHostPorts {
		if name != "radiology" && (port == RadiologyHostPort || port == RadiologyDICOMPort) {
			t.Errorf("radiology collides with managed %s on %d", name, port)
		}
	}
	for port, name := range ReservedCitadelPorts {
		if port == RadiologyHostPort || port == RadiologyDICOMPort {
			t.Errorf("radiology collides with reserved %s on %d", name, port)
		}
	}
	if got := ClaimedHostPorts()[RadiologyDICOMPort]; got != "radiology-dicom" {
		t.Errorf("DICOM default must be claimed against dynamic allocation, got %q", got)
	}
	want := fmt.Sprintf("%s=%d", EnvRadiologyHostPort, RadiologyHostPort)
	if !strings.Contains(strings.Join(HostPortEnv(), "\n"), want) {
		t.Errorf("HostPortEnv missing %s", want)
	}
}

// Execute the exact compose healthcheck source, not a second implementation of
// its decision logic. Python3 with its stdlib is a REQUIRED test prerequisite:
// missing coverage fails CI instead of being hidden behind a skip. All HTTP is
// injected; the child receives no inherited host credentials or configuration.
func TestRadiologyHealthProbeSyntheticPython(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 with its standard library is required for the exact radiology healthcheck contract test")
	}
	var compose struct {
		Services map[string]struct {
			Healthcheck struct {
				Test []string `yaml:"test"`
			} `yaml:"healthcheck"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(radiologyComposeYAML, &compose); err != nil {
		t.Fatal(err)
	}
	probe := compose.Services["orthanc"].Healthcheck.Test
	if len(probe) != 7 || strings.Join(probe[:6], ",") != "CMD,python3,-I,-S,-B,-c" {
		t.Fatal("healthcheck argv changed; update the exact-source test explicitly")
	}
	const sentinel = "synthetic-radiology-password-never-print"
	valid := `{"Version":"synthetic-version","Name":"` + sentinel + `"}`
	type scenario struct {
		Name            string `json:"name"`
		Status          int    `json:"status"`
		Body            string `json:"body"`
		Error           string `json:"error"`
		MissingPassword bool   `json:"missing_password"`
		EmptyPassword   bool   `json:"empty_password"`
		WantCode        int    `json:"want_code"`
	}
	for _, tc := range []scenario{
		{Name: "recognized-200", Status: 200, Body: valid, WantCode: 0},
		{Name: "unauthorized", Status: 401, Body: sentinel, WantCode: 1},
		{Name: "forbidden", Status: 403, Body: sentinel, WantCode: 1},
		{Name: "http-error-401", Error: "http401", WantCode: 1},
		{Name: "http-error-403", Error: "http403", WantCode: 1},
		{Name: "redirect", Status: 302, Body: valid, WantCode: 1},
		{Name: "oversized", Status: 200, Body: strings.Repeat("x", 65537), WantCode: 1},
		{Name: "malformed", Status: 200, Body: sentinel, WantCode: 1},
		{Name: "nonobject", Status: 200, Body: `[]`, WantCode: 1},
		{Name: "missing-version", Status: 200, Body: `{}`, WantCode: 1},
		{Name: "numeric-version", Status: 200, Body: `{"Version":123}`, WantCode: 1},
		{Name: "empty-version", Status: 200, Body: `{"Version":""}`, WantCode: 1},
		{Name: "missing-password", MissingPassword: true, WantCode: 1},
		{Name: "empty-password", EmptyPassword: true, WantCode: 1},
		{Name: "network-exception", Error: "network", WantCode: 1},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			encoded, err := json.Marshal(tc)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, python, "-I", "-S", "-B", "-c", radiologyProbeSyntheticHarness, string(encoded), probe[6])
			cmd.WaitDelay = time.Second
			privateDir := t.TempDir()
			cmd.Dir = privateDir
			cmd.Env = []string{"HOME=" + privateDir, "TMPDIR=" + privateDir}
			if !tc.MissingPassword {
				password := sentinel
				if tc.EmptyPassword {
					password = ""
				}
				cmd.Env = append(cmd.Env, "ORTHANC_PASSWORD="+password)
			}
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("synthetic healthcheck test exceeded its deadline")
			}
			if err != nil {
				t.Fatalf("exact healthcheck synthetic contract failed: %v (output withheld)", err)
			}
			if len(output) != 0 {
				t.Fatalf("synthetic harness emitted %d bytes; healthcheck must be silent", len(output))
			}
		})
	}
}

// The harness checks transport construction and observed behavior separately
// from the probe's expected exit. A swallowed mock assertion cannot turn a
// negative case green. urllib/socket fallback paths fail without real network.
const radiologyProbeSyntheticHarness = `
import base64, contextlib, io, json, socket, sys, urllib.error, urllib.request
case = json.loads(sys.argv[1])
source = sys.argv[2]
sentinel = "synthetic-radiology-password-never-print"
violations = []
events = {"build": 0, "open": 0, "read": 0}

def violation(reason):
    violations.append(reason)
    raise RuntimeError("synthetic contract violation")

def refuse_network(*args, **kwargs):
    violation("unexpected real network path")

socket.socket = refuse_network
socket.create_connection = refuse_network
socket.getaddrinfo = refuse_network
urllib.request.urlopen = refuse_network

class Response:
    status = case["status"]
    def __enter__(self):
        return self
    def __exit__(self, *args):
        return False
    def read(self, limit):
        events["read"] += 1
        if limit != 65537:
            violation("response read must use the declared bound")
        return case["body"].encode()[:limit]

class Opener:
    def open(self, request, timeout=None):
        events["open"] += 1
        if request.full_url != "http://127.0.0.1:8042/system" or request.get_method() != "GET" or timeout != 5:
            violation("fixed request or timeout changed")
        expected = "Basic " + base64.b64encode(("orthanc:" + sentinel).encode()).decode()
        if request.get_header("Authorization") != expected:
            violation("fixed-user Basic Auth absent or changed")
        if case["error"].startswith("http"):
            raise urllib.error.HTTPError(request.full_url, int(case["error"][4:]), sentinel, {}, io.BytesIO(sentinel.encode()))
        if case["error"] == "network":
            raise urllib.error.URLError(sentinel)
        return Response()

def build_opener(*handlers):
    events["build"] += 1
    if len(handlers) != 2 or not isinstance(handlers[0], urllib.request.ProxyHandler) or handlers[0].proxies != {}:
        violation("proxy construction is not explicitly empty")
    if not isinstance(handlers[1], urllib.request.HTTPRedirectHandler):
        violation("redirect handler missing")
    redirect = handlers[1].redirect_request(urllib.request.Request("http://127.0.0.1:8042/system"), None, 302, "synthetic", {}, "http://127.0.0.1:8042/redirect")
    if redirect is not None:
        violation("redirect handler permits a redirect")
    return Opener()

urllib.request.build_opener = build_opener
out, err = io.StringIO(), io.StringIO()
code = 0
with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
    try:
        exec(compile(source, "<exact-compose-healthcheck>", "exec"), {"__name__": "__main__"})
    except SystemExit as exit_result:
        code = exit_result.code
    except BaseException:
        violations.append("probe escaped its silent error boundary")

no_password = case["missing_password"] or case["empty_password"]
expected_calls = 0 if no_password else 1
expected_reads = 0 if no_password or case["error"] else 1
if events != {"build": expected_calls, "open": expected_calls, "read": expected_reads}:
    violations.append("probe did not exercise the required exact path")
if violations or code != case["want_code"] or out.getvalue() or err.getvalue():
    sys.exit(86)
`
