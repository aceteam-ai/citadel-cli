package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

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
