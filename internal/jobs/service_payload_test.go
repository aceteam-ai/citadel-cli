// internal/jobs/service_payload_test.go
package jobs

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	embeddedservices "github.com/aceteam-ai/citadel-cli/services"
)

const testHome = "/home/citadel"

// basePayload is a valid extended SERVICE_START payload as the platform sends
// it (aceteam/python-backend/utils/instance_node_dispatch.py). env arrives as a
// JSON string because the worker->nexus.Job adapter json-encodes nested values.
func basePayload() map[string]string {
	return map[string]string{
		"service":           "ac-pond-steel-bone-6655",
		"instance_id":       "11111111-2222-3333-4444-555555555555",
		"image":             "ghcr.io/aceteam-ai/claudecode-service:latest",
		"env":               `{"ANTHROPIC_BASE_URL":"https://proxy.example","ACETEAM_INSTANCE_ID":"i-1"}`,
		"host_port":         "18789",
		"state_volume_path": "~/citadel-cache/instances/i-1",
		"state_mount_path":  "/state",
	}
}

func TestPayloadHasInlineSpec(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]string
		want    bool
	}{
		{"extended with image", map[string]string{"service": "ac-x", "image": "ghcr.io/aceteam-ai/x"}, true},
		{"legacy name-only", map[string]string{"service": "vllm"}, false},
		{"blank image", map[string]string{"service": "vllm", "image": "  "}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := payloadHasInlineSpec(tt.payload); got != tt.want {
				t.Errorf("payloadHasInlineSpec = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseInstanceSpec_Valid(t *testing.T) {
	spec, err := parseInstanceSpec(basePayload(), testHome)
	if err != nil {
		t.Fatalf("parseInstanceSpec: %v", err)
	}
	if spec.ServiceName != "ac-pond-steel-bone-6655" {
		t.Errorf("ServiceName = %q", spec.ServiceName)
	}
	if spec.ContainerName != "citadel-ac-pond-steel-bone-6655" {
		t.Errorf("ContainerName = %q", spec.ContainerName)
	}
	if spec.HostPort != 18789 {
		t.Errorf("HostPort = %d, want 18789", spec.HostPort)
	}
	// No PORT in env -> wrapper default container port.
	if spec.ContainerPort != defaultContainerPort {
		t.Errorf("ContainerPort = %d, want %d", spec.ContainerPort, defaultContainerPort)
	}
	// ~ expanded and bounded to the citadel data dir.
	wantVol := filepath.Join(testHome, "citadel-cache", "instances", "i-1")
	if spec.StateVolumePath != wantVol {
		t.Errorf("StateVolumePath = %q, want %q", spec.StateVolumePath, wantVol)
	}
	if spec.StateMountPath != "/state" {
		t.Errorf("StateMountPath = %q", spec.StateMountPath)
	}
	wantEnv := map[string]string{"ANTHROPIC_BASE_URL": "https://proxy.example", "ACETEAM_INSTANCE_ID": "i-1"}
	if !reflect.DeepEqual(spec.Env, wantEnv) {
		t.Errorf("Env = %v, want %v", spec.Env, wantEnv)
	}
}

func TestParseInstanceSpec_Defaults(t *testing.T) {
	p := basePayload()
	delete(p, "state_mount_path") // omitted -> default /state
	p["env"] = `{"PORT":"9000"}`  // explicit container port
	spec, err := parseInstanceSpec(p, testHome)
	if err != nil {
		t.Fatalf("parseInstanceSpec: %v", err)
	}
	if spec.StateMountPath != defaultStateMountPath {
		t.Errorf("StateMountPath = %q, want %q", spec.StateMountPath, defaultStateMountPath)
	}
	if spec.ContainerPort != 9000 {
		t.Errorf("ContainerPort = %d, want 9000 (from env PORT)", spec.ContainerPort)
	}
}

func TestParseInstanceSpec_Rejects(t *testing.T) {
	reservedPort := "" // filled below with a real reserved port
	for p := range embeddedservices.ReservedCitadelPorts {
		reservedPort = itoa(p)
		break
	}

	tests := []struct {
		name   string
		mutate func(map[string]string)
		errSub string
	}{
		{"missing service", func(p map[string]string) { delete(p, "service") }, "missing 'service'"},
		{"service starts with dash", func(p map[string]string) { p["service"] = "-evil" }, "must not start with '-'"},
		{"service bad chars", func(p map[string]string) { p["service"] = "ac/../x" }, "only [A-Za-z0-9._-]"},
		{"image not allowed registry", func(p map[string]string) { p["image"] = "docker.io/library/nginx" }, "not from an allowed registry"},
		{"image flag injection", func(p map[string]string) { p["image"] = "-v/etc:/etc" }, "must not start with '-'"},
		{"missing image", func(p map[string]string) { delete(p, "image") }, "missing 'image'"},
		{"bad env json", func(p map[string]string) { p["env"] = "not-json" }, "invalid env payload"},
		{"empty env key", func(p map[string]string) { p["env"] = `{"":"v"}` }, "empty key"},
		{"host_port not a number", func(p map[string]string) { p["host_port"] = "abc" }, "invalid host_port"},
		{"host_port reserved", func(p map[string]string) { p["host_port"] = reservedPort }, "reserved for citadel"},
		{"volume outside data dir", func(p map[string]string) { p["state_volume_path"] = "/etc" }, "outside the citadel data dir"},
		{"volume traversal via ~", func(p map[string]string) { p["state_volume_path"] = "~/../../etc/passwd" }, "outside the citadel data dir"},
		{"relative mount path", func(p map[string]string) { p["state_mount_path"] = "state" }, "must be an absolute container path"},
		{"runtime not allowed", func(p map[string]string) { p["runtime"] = "runsc-evil" }, "invalid runtime"},
		{"runtime shell injection", func(p map[string]string) { p["runtime"] = "kata; rm -rf /" }, "invalid runtime"},
		{"runtime flag injection", func(p map[string]string) { p["runtime"] = "--privileged" }, "invalid runtime"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := basePayload()
			tt.mutate(p)
			_, err := parseInstanceSpec(p, testHome)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.errSub)
			}
			if !strings.Contains(err.Error(), tt.errSub) {
				t.Errorf("error = %q, want substring %q", err.Error(), tt.errSub)
			}
		})
	}
}

func TestResolveStateVolumePath(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"tilde slash under cache", "~/citadel-cache/instances/x", filepath.Join(testHome, "citadel-cache/instances/x"), false},
		{"bare tilde is home not allowed", "~", "", true},
		{"absolute under .citadel", filepath.Join(testHome, ".citadel/instances/x"), filepath.Join(testHome, ".citadel/instances/x"), false},
		{"escape to etc", "/etc/passwd", "", true},
		{"prefix sibling not matched", filepath.Join(testHome, "citadel-cache-evil/x"), "", true},
		{"empty", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveStateVolumePath(tt.raw, testHome)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateImageRef(t *testing.T) {
	tests := []struct {
		image   string
		wantErr bool
	}{
		{"ghcr.io/aceteam-ai/claudecode-service:latest", false},
		{"ghcr.io/aceteam-ai/hermes@sha256:abc", false},
		{"docker.io/library/nginx", true},
		{"ghcr.io/evil-org/backdoor", true},
		{"", true},
		{"-malicious", true},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			err := validateImageRef(tt.image)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateImageRef(%q) err = %v, wantErr %v", tt.image, err, tt.wantErr)
			}
		})
	}
}

func TestBuildDockerRunArgs(t *testing.T) {
	spec := &instanceSpec{
		ServiceName:     "ac-x",
		ContainerName:   "citadel-ac-x",
		Image:           "ghcr.io/aceteam-ai/claudecode-service:latest",
		Env:             map[string]string{"ANTHROPIC_BASE_URL": "https://p"},
		HostPort:        18789,
		ContainerPort:   8787,
		StateVolumePath: "/home/citadel/citadel-cache/instances/i-1",
		StateMountPath:  "/state",
	}
	args := buildDockerRunArgs(spec)
	joined := strings.Join(args, " ")

	// Image must be the final arg (never read as a flag).
	if args[len(args)-1] != spec.Image {
		t.Errorf("image is not the last arg: %v", args)
	}
	wantContains := []string{
		"--name citadel-ac-x",
		"--restart unless-stopped",
		"-p 127.0.0.1:18789:8787",
		"-v /home/citadel/citadel-cache/instances/i-1:/state",
		"-e CLAUDE_CONFIG_DIR=/state", // durability injection
		"-e PORT=8787",                // publish-port injection
		"-e ANTHROPIC_BASE_URL=https://p",
	}
	for _, want := range wantContains {
		if !strings.Contains(joined, want) {
			t.Errorf("docker run args missing %q\n got: %s", want, joined)
		}
	}
}

func TestBuildDockerRunArgs_PayloadEnvWins(t *testing.T) {
	// If the payload already sets CLAUDE_CONFIG_DIR/PORT, do not double-inject.
	spec := &instanceSpec{
		ServiceName:     "ac-x",
		ContainerName:   "citadel-ac-x",
		Image:           "ghcr.io/aceteam-ai/claudecode-service:latest",
		Env:             map[string]string{"CLAUDE_CONFIG_DIR": "/custom", "PORT": "8787"},
		HostPort:        18789,
		ContainerPort:   8787,
		StateVolumePath: "/home/citadel/citadel-cache/instances/i-1",
		StateMountPath:  "/state",
	}
	args := buildDockerRunArgs(spec)
	n := 0
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-e" && strings.HasPrefix(args[i+1], "CLAUDE_CONFIG_DIR=") {
			n++
			if args[i+1] != "CLAUDE_CONFIG_DIR=/custom" {
				t.Errorf("payload CLAUDE_CONFIG_DIR not honored: %q", args[i+1])
			}
		}
	}
	if n != 1 {
		t.Errorf("expected exactly one CLAUDE_CONFIG_DIR env, got %d", n)
	}
}

func TestValidateRuntime(t *testing.T) {
	tests := []struct {
		runtime string
		wantErr bool
	}{
		{"", false},             // empty -> daemon default, no flag
		{"kata", false},         // allowed
		{"kata-runtime", false}, // allowed
		{"runsc", false},        // allowed
		{"runc", true},          // not on the allowlist (default is the empty string, not "runc")
		{"nvidia", true},        // not on the allowlist
		{"runsc-evil", true},    // near-miss must not pass
		{"kata; rm -rf /", true},
		{"--privileged", true},
		{"kata runsc", true},
	}
	for _, tt := range tests {
		t.Run(tt.runtime, func(t *testing.T) {
			err := validateRuntime(tt.runtime)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateRuntime(%q) err = %v, wantErr %v", tt.runtime, err, tt.wantErr)
			}
		})
	}
}

func TestParseInstanceSpec_Runtime(t *testing.T) {
	p := basePayload()
	p["runtime"] = "kata"
	spec, err := parseInstanceSpec(p, testHome)
	if err != nil {
		t.Fatalf("parseInstanceSpec: %v", err)
	}
	if spec.Runtime != "kata" {
		t.Errorf("Runtime = %q, want kata", spec.Runtime)
	}

	// Omitted runtime -> empty (daemon default).
	spec2, err := parseInstanceSpec(basePayload(), testHome)
	if err != nil {
		t.Fatalf("parseInstanceSpec: %v", err)
	}
	if spec2.Runtime != "" {
		t.Errorf("Runtime = %q, want empty", spec2.Runtime)
	}
}

func TestBuildDockerRunArgs_Runtime(t *testing.T) {
	base := func() *instanceSpec {
		return &instanceSpec{
			ServiceName:     "ac-x",
			ContainerName:   "citadel-ac-x",
			Image:           "ghcr.io/aceteam-ai/claudecode-service:latest",
			Env:             map[string]string{},
			HostPort:        18789,
			ContainerPort:   8787,
			StateVolumePath: "/home/citadel/citadel-cache/instances/i-1",
			StateMountPath:  "/state",
		}
	}

	// With runtime: exactly one --runtime=<value> flag, image still last.
	withRT := base()
	withRT.Runtime = "runsc"
	args := buildDockerRunArgs(withRT)
	if args[len(args)-1] != withRT.Image {
		t.Errorf("image is not the last arg: %v", args)
	}
	n := 0
	for _, a := range args {
		if a == "--runtime=runsc" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected exactly one --runtime=runsc arg, got %d in %v", n, args)
	}

	// Without runtime: no --runtime flag at all.
	noRT := base()
	got := strings.Join(buildDockerRunArgs(noRT), " ")
	if strings.Contains(got, "--runtime") {
		t.Errorf("did not expect a --runtime flag, got: %s", got)
	}
}

func TestPreflightRuntime(t *testing.T) {
	// Empty runtime never touches the daemon and always passes.
	orig := dockerRuntimesFunc
	defer func() { dockerRuntimesFunc = orig }()

	dockerRuntimesFunc = func() (map[string]bool, error) {
		t.Fatal("preflightRuntime queried the daemon for an empty runtime")
		return nil, nil
	}
	if err := preflightRuntime(""); err != nil {
		t.Errorf("preflightRuntime(\"\") = %v, want nil", err)
	}

	// Requested runtime registered with the daemon -> pass.
	dockerRuntimesFunc = func() (map[string]bool, error) {
		return map[string]bool{"runc": true, "kata-runtime": true}, nil
	}
	if err := preflightRuntime("kata-runtime"); err != nil {
		t.Errorf("preflightRuntime(kata-runtime) = %v, want nil", err)
	}

	// Requested runtime absent -> actionable failure. A "kata" request is NOT
	// satisfied by a daemon that only registered "kata-runtime" (verbatim check).
	dockerRuntimesFunc = func() (map[string]bool, error) {
		return map[string]bool{"runc": true, "kata-runtime": true}, nil
	}
	err := preflightRuntime("kata")
	if err == nil {
		t.Fatal("expected preflight failure for absent runtime, got nil")
	}
	if !strings.Contains(err.Error(), "not installed on this node") {
		t.Errorf("error = %q, want 'not installed on this node'", err.Error())
	}

	// Daemon query error propagates.
	dockerRuntimesFunc = func() (map[string]bool, error) {
		return nil, errStub
	}
	if err := preflightRuntime("runsc"); err == nil {
		t.Error("expected preflight to propagate daemon query error, got nil")
	}
}

// errStub is a sentinel error for the daemon-query failure path.
var errStub = stubErr("docker daemon unreachable")

type stubErr string

func (e stubErr) Error() string { return string(e) }

// itoa avoids importing strconv just for the reserved-port table above.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
