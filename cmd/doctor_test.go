package cmd

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

func TestRunPodmanGPUProbe(t *testing.T) {
	var gotBinary string
	var gotArgs []string
	health := runPodmanGPUProbe(
		catalog.ContainerRuntime{EngineBin: "podman", Rootless: true},
		func(string) (string, error) { return "/usr/bin/nvidia-smi", nil },
		func(_ context.Context, binary string, args ...string) ([]byte, error) {
			gotBinary, gotArgs = binary, append([]string(nil), args...)
			return []byte("GPU 0: Test GPU"), nil
		},
	)
	if !health.Applicable || !health.OK {
		t.Fatalf("health = %+v, want applicable and OK", health)
	}
	want := []string{"run", "--rm", "--pull=never", "--device", "nvidia.com/gpu=all", "--security-opt=label=disable", doctorCUDAProbeImage, "nvidia-smi", "-L"}
	if gotBinary != "podman" || !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("command = %s %v, want podman %v", gotBinary, gotArgs, want)
	}
}

func TestRunPodmanGPUProbeFailureAndNotApplicable(t *testing.T) {
	failed := runPodmanGPUProbe(
		catalog.ContainerRuntime{EngineBin: "podman", Rootless: true},
		func(string) (string, error) { return "/usr/bin/nvidia-smi", nil },
		func(context.Context, string, ...string) ([]byte, error) {
			return []byte("CDI device unavailable"), errors.New("exit 125")
		},
	)
	if !failed.Applicable || failed.OK || !strings.Contains(failed.Message, "CDI device unavailable") {
		t.Fatalf("failed health = %+v", failed)
	}

	none := runPodmanGPUProbe(
		catalog.ContainerRuntime{EngineBin: "podman", Rootless: true},
		func(string) (string, error) { return "", errors.New("missing") },
		func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("runner called without host GPU")
			return nil, nil
		},
	)
	if none.Applicable || none.OK {
		t.Fatalf("no-GPU health = %+v, want not applicable", none)
	}
}

func TestDoctorReportOKIncludesPodmanSafetyChecks(t *testing.T) {
	base := doctorReport{dockerHealth: platform.DockerHealth{OK: true}, doctor: healthyDoctorPayload()}
	base.cgroupHealth = platform.CgroupDelegationHealth{Applicable: true, OK: false, Missing: []string{"memory"}}
	if base.ok() {
		t.Fatal("missing cgroup delegation must fail doctor")
	}
	base.cgroupHealth.OK = true
	base.gpuHealth = gpuProbeHealth{Applicable: true, OK: false, Message: "CDI failed"}
	if base.ok() {
		t.Fatal("failed CDI probe must fail doctor")
	}
}

// healthyDoctorPayload builds an agentDoctor-shaped payload for a fully
// healthy job-routing state, mirroring the map shape agentDoctor
// (cmd/agent_tools.go) actually returns.
func healthyDoctorPayload() map[string]any {
	return map[string]any{
		"healthy": true,
		"checks": []map[string]any{
			{"name": "headscale_node_id_resolved", "ok": true, "detail": "node-123"},
			{"name": "org_id_known", "ok": true, "detail": "org-456"},
		},
		"diagnosis": "Node looks healthy for per-node job routing.",
	}
}

// unhealthyDoctorPayload builds a payload matching what agentDoctor(worker.WorkerSnapshot{})
// actually produces: every identity/subscription field is empty and
// Consuming is false, so every check reads unhealthy.
func unhealthyDoctorPayload() map[string]any {
	return map[string]any{
		"healthy": false,
		"checks": []map[string]any{
			{"name": "headscale_node_id_resolved", "ok": false, "detail": "unresolved — this node declines every target_node-addressed job"},
			{"name": "org_id_known", "ok": false, "detail": "unknown (per-node stream skipped)"},
			{"name": "worker_consuming", "ok": false, "detail": "last poll: never"},
		},
		"diagnosis": "Headscale node ID is unresolved, so the per-node shell stream was never subscribed.",
	}
}

func TestDoctorReportOK_HealthyDocker(t *testing.T) {
	r := doctorReport{
		dockerHealth: platform.DockerHealth{OK: true},
		doctor:       healthyDoctorPayload(),
	}
	if !r.ok() {
		t.Fatalf("expected ok() to be true when docker is healthy")
	}
}

func TestDoctorReportOK_UnhealthyDocker(t *testing.T) {
	r := doctorReport{
		dockerHealth: platform.DockerHealth{
			OK:      false,
			Code:    "cli_missing",
			Message: "docker CLI not found on PATH.",
			Hint:    "Install it with 'brew install docker'.",
		},
		doctor: healthyDoctorPayload(),
	}
	if r.ok() {
		t.Fatalf("expected ok() to be false when docker is unhealthy, regardless of job-routing health")
	}
}

// TestDoctorReportOK_JobRoutingDoesNotAffectExitCode pins the deliberate
// design decision documented on doctorReport.ok: the job-routing/worker
// section is informational only for a standalone CLI invocation (no live
// worker to introspect), so an "unhealthy" agentDoctor payload must NOT flip
// ok() to false on its own.
func TestDoctorReportOK_JobRoutingDoesNotAffectExitCode(t *testing.T) {
	r := doctorReport{
		dockerHealth: platform.DockerHealth{OK: true},
		doctor:       unhealthyDoctorPayload(),
	}
	if !r.ok() {
		t.Fatalf("expected ok() to be true: docker is healthy, job-routing is informational-only")
	}
}

func TestRenderDoctorReport_Healthy(t *testing.T) {
	var buf bytes.Buffer
	r := doctorReport{
		dockerHealth: platform.DockerHealth{OK: true},
		doctor:       healthyDoctorPayload(),
	}
	renderDoctorReport(&buf, r)
	out := buf.String()

	for _, want := range []string{
		"Citadel Doctor",
		"DOCKER / ENGINE",
		"[OK]",
		"docker/engine usable",
		"JOB ROUTING / WORKER HEALTH",
		"headscale_node_id_resolved",
		"Diagnosis:",
		"Node looks healthy for per-node job routing.",
		"Overall: OK",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered report missing %q\nfull output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "PROBLEMS DETECTED") {
		t.Errorf("healthy report should not claim problems were detected:\n%s", out)
	}
}

func TestRenderDoctorReport_UnhealthyDocker(t *testing.T) {
	var buf bytes.Buffer
	r := doctorReport{
		dockerHealth: platform.DockerHealth{
			OK:      false,
			Code:    "cli_missing",
			Message: "docker CLI not found on PATH.",
			Hint:    "Install it with 'brew install docker'.",
		},
		doctor: unhealthyDoctorPayload(),
	}
	renderDoctorReport(&buf, r)
	out := buf.String()

	for _, want := range []string{
		"[FAIL]",
		"docker CLI not found on PATH.",
		"Install it with 'brew install docker'.",
		"PROBLEMS DETECTED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered report missing %q\nfull output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Overall: OK") {
		t.Errorf("unhealthy report should not claim OK:\n%s", out)
	}
}

// dockerUsableCheck mirrors the "docker_usable" entry agentDoctor
// (cmd/agent_tools.go) folds into its "checks" slice via its own internal
// platform.CheckDockerUsable probe.
func dockerUsableCheck(ok bool, detail string) map[string]any {
	return map[string]any{"name": "docker_usable", "ok": ok, "detail": detail}
}

// TestRenderDoctorReport_DockerCheckShownOnce pins the fix for citadel-cli#803:
// agentDoctor's own "checks" slice includes a "docker_usable" entry (it runs
// its own platform.CheckDockerUsable probe for the /agent/doctor HTTP
// endpoint / citadel_doctor MCP tool, whose contract this test must not
// disturb), but the `doctor` command's DOCKER / ENGINE section already
// renders that exact signal from r.dockerHealth. Without de-duping, the
// rendered report showed the docker check twice per invocation. This test
// builds a doctor payload shaped like the real agentDoctor output (checks
// includes "docker_usable") and asserts the human report shows it once.
func TestRenderDoctorReport_DockerCheckShownOnce(t *testing.T) {
	dockerMsg := "docker CLI not found on PATH."
	r := doctorReport{
		dockerHealth: platform.DockerHealth{
			OK:      false,
			Code:    "cli_missing",
			Message: dockerMsg,
			Hint:    "Install it with 'brew install docker'.",
		},
		doctor: map[string]any{
			"healthy": false,
			"checks": []map[string]any{
				{"name": "headscale_node_id_resolved", "ok": true, "detail": "node-123"},
				dockerUsableCheck(false, dockerMsg),
			},
			"diagnosis": "Node looks healthy for per-node job routing.",
		},
	}

	var buf bytes.Buffer
	renderDoctorReport(&buf, r)
	out := buf.String()

	if n := strings.Count(out, "DOCKER / ENGINE"); n != 1 {
		t.Errorf("expected exactly one DOCKER / ENGINE section, got %d:\n%s", n, out)
	}
	if n := strings.Count(out, dockerMsg); n != 1 {
		t.Errorf("expected the docker health message to appear exactly once, got %d:\n%s", n, out)
	}
	if strings.Contains(out, "docker_usable") {
		t.Errorf("expected the job-routing section to suppress the duplicate docker_usable line:\n%s", out)
	}
	// Non-docker job-routing checks must still render.
	if !strings.Contains(out, "headscale_node_id_resolved") {
		t.Errorf("expected non-docker job-routing checks to still render:\n%s", out)
	}
}

// TestRunDoctorChecksNoCrash exercises the real wiring (agentDoctor +
// platform.CheckDockerUsable) end to end. It intentionally does not assert on
// docker's presence/absence -- CI/dev machines differ -- only that gathering
// the report never panics and returns a well-formed payload for a zero-value
// (no live worker) snapshot.
func TestRunDoctorChecksNoCrash(t *testing.T) {
	report := runDoctorChecks()

	if report.doctor == nil {
		t.Fatalf("expected a non-nil doctor payload")
	}
	if _, ok := report.doctor["checks"].([]map[string]any); !ok {
		t.Fatalf("expected doctor payload to carry a []map[string]any \"checks\" field, got %T", report.doctor["checks"])
	}
	if _, ok := report.doctor["worker"].(worker.WorkerSnapshot); !ok {
		t.Fatalf("expected doctor payload to echo back the worker.WorkerSnapshot it was given, got %T", report.doctor["worker"])
	}

	// Rendering must not panic regardless of the local docker/engine state.
	var buf bytes.Buffer
	renderDoctorReport(&buf, report)

	// citadel-cli#803: end-to-end (real agentDoctor output, which does
	// include a "docker_usable" check), the rendered report must show the
	// docker/engine check exactly once -- in DOCKER / ENGINE -- not also as a
	// "docker_usable" line under JOB ROUTING / WORKER HEALTH.
	out := buf.String()
	if n := strings.Count(out, "DOCKER / ENGINE"); n != 1 {
		t.Errorf("expected exactly one DOCKER / ENGINE section, got %d:\n%s", n, out)
	}
	if strings.Contains(out, "docker_usable") {
		t.Errorf("expected the job-routing section to suppress the duplicate docker_usable line:\n%s", out)
	}

	if buf.Len() == 0 {
		t.Fatalf("expected renderDoctorReport to write output")
	}
}

// TestRenderDoctorReport_BindExposureWarns pins the aceteam-ai/citadel-cli#1023
// section: an engine published on all interfaces renders a [WARN] line under
// ENGINE NETWORK EXPOSURE, but (like the job-routing section) does NOT flip the
// exit code -- a deliberate `bind: all` is a valid operator choice.
func TestRenderDoctorReport_BindExposureWarns(t *testing.T) {
	var buf bytes.Buffer
	r := doctorReport{
		dockerHealth: platform.DockerHealth{OK: true},
		doctor:       healthyDoctorPayload(),
		bindWarnings: []string{"vllm is published on ALL network interfaces (0.0.0.0) with no authentication of its own; set `bind: loopback` ..."},
	}
	if !r.ok() {
		t.Fatalf("bind exposure warnings must NOT flip ok() to false")
	}
	renderDoctorReport(&buf, r)
	out := buf.String()
	for _, want := range []string{"ENGINE NETWORK EXPOSURE", "[WARN]", "vllm is published on ALL network interfaces", "Overall: OK"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered report missing %q\nfull output:\n%s", want, out)
		}
	}
}

// TestRenderDoctorReport_NoBindExposure pins the OK line when no engine is
// exposed on all interfaces.
func TestRenderDoctorReport_NoBindExposure(t *testing.T) {
	var buf bytes.Buffer
	r := doctorReport{
		dockerHealth: platform.DockerHealth{OK: true},
		doctor:       healthyDoctorPayload(),
	}
	renderDoctorReport(&buf, r)
	out := buf.String()
	if !strings.Contains(out, "ENGINE NETWORK EXPOSURE") {
		t.Errorf("expected the ENGINE NETWORK EXPOSURE section:\n%s", out)
	}
	if !strings.Contains(out, "no embedded engine is published on all interfaces") {
		t.Errorf("expected the no-exposure OK line:\n%s", out)
	}
}

func TestRenderDoctorReport_EphemeralServiceExecWarns(t *testing.T) {
	var buf bytes.Buffer
	r := doctorReport{
		dockerHealth: platform.DockerHealth{OK: true},
		doctor:       healthyDoctorPayload(),
		serviceExecWarnings: []string{
			"/etc/systemd/system/citadel.service runs /tmp/citadel from ephemeral /tmp; reinstall before reboot",
		},
	}
	if !r.ok() {
		t.Fatal("an unsafe service path must be a warning, not hide docker health")
	}
	renderDoctorReport(&buf, r)
	out := buf.String()
	for _, want := range []string{"SERVICE EXECUTABLE", "[WARN]", "/tmp/citadel", "Overall: OK"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered report missing %q\nfull output:\n%s", want, out)
		}
	}
}

// TestDoctorReportOK_DarwinDockerOptional pins citadel-cli#1042: on macOS Docker
// Desktop is optional, so an unusable engine must NOT fail the exit code.
func TestDoctorReportOK_DarwinDockerOptional(t *testing.T) {
	r := doctorReport{
		dockerHealth: platform.DockerHealth{
			OK:      false,
			Code:    "cli_missing",
			Message: "docker CLI not found on PATH.",
		},
		doctor:         healthyDoctorPayload(),
		dockerOptional: true,
	}
	if !r.ok() {
		t.Fatalf("expected ok()=true on darwin (Docker optional) even with docker unusable")
	}
}

// TestRunDoctorChecksForWiresDockerOptional pins that runDoctorChecks resolves
// dockerOptional FROM the isDarwin signal (not a hardcoded false), so a future
// refactor dropping that wiring is caught even though CI runs on Linux where the
// darwin branch never executes end-to-end.
func TestRunDoctorChecksForWiresDockerOptional(t *testing.T) {
	if r := runDoctorChecksFor(true); !r.dockerOptional {
		t.Errorf("runDoctorChecksFor(true) must set dockerOptional")
	}
	if r := runDoctorChecksFor(false); r.dockerOptional {
		t.Errorf("runDoctorChecksFor(false) must not set dockerOptional")
	}
}

// TestRenderDoctorReport_DarwinDockerOptional pins the render: an unusable
// engine on darwin is a [WARN] with an explanatory note and an overall OK, not
// a [FAIL]/PROBLEMS DETECTED.
func TestRenderDoctorReport_DarwinDockerOptional(t *testing.T) {
	var buf bytes.Buffer
	r := doctorReport{
		dockerHealth: platform.DockerHealth{
			OK:      false,
			Code:    "cli_missing",
			Message: "docker CLI not found on PATH.",
		},
		doctor:         healthyDoctorPayload(),
		dockerOptional: true,
	}
	renderDoctorReport(&buf, r)
	out := buf.String()

	for _, want := range []string{"[WARN]", "optional on macOS", "Overall: OK"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered darwin report missing %q\nfull output:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"[FAIL]", "PROBLEMS DETECTED"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("darwin (docker optional) report should not contain %q\nfull output:\n%s", unwanted, out)
		}
	}
}
