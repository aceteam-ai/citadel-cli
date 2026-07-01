package nodestate

import (
	"context"
	"os/exec"
	"strings"
	"time"

	fabricpb "github.com/aceteam-ai/fabric-protocol/gen/go/aceteam/fabric/v1"
)

// dockerInspector observes module run-state via `docker inspect` on the
// conventionally-named container (citadel-<module>, matching
// internal/jobs.ServiceHandler). It implements ModuleInspector.
type dockerInspector struct{}

// DockerInspector returns the live docker-backed ModuleInspector. If docker is
// not on PATH it returns nil, and BuildActualState then reports every module as
// status/health UNSPECIFIED rather than ERROR — "I can't observe run-state" is
// not a per-module failure.
func DockerInspector() ModuleInspector {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil
	}
	return dockerInspector{}
}

const inspectTimeout = 3 * time.Second

// Inspect maps the container's docker state to a ModuleStatus/ModuleHealth.
//
// A missing container is NOT an error: a module that was cleanly stopped (or
// whose container was removed) reports STOPPED, so a normal stopped module does
// not spam the report with ERROR. An error is returned only when docker itself
// fails in a way that leaves run-state genuinely unknown — that is the path that
// surfaces as MODULE_HEALTH_ERROR in the report, isolated to this one module.
func (dockerInspector) Inspect(ctx context.Context, moduleName string) (Observation, error) {
	container := "citadel-" + moduleName

	ctx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{end}}",
		container).Output()
	if err != nil {
		// "No such object" => the container is absent. Treat as STOPPED (the
		// module is installed per the lockfile but not running), NOT an error.
		if isNoSuchContainer(err) {
			return Observation{
				Status: fabricpb.ModuleStatus_MODULE_STATUS_STOPPED,
				Health: fabricpb.ModuleHealth_MODULE_HEALTH_UNSPECIFIED,
			}, nil
		}
		return Observation{}, err
	}

	dockerStatus, healthStatus, _ := strings.Cut(strings.TrimSpace(string(out)), "|")
	return Observation{
		Status: mapStatus(dockerStatus),
		Health: mapHealth(dockerStatus, healthStatus),
	}, nil
}

// isNoSuchContainer reports whether a docker inspect error is the benign
// "container does not exist" case, as opposed to a real docker failure.
func isNoSuchContainer(err error) bool {
	if exitErr, ok := err.(*exec.ExitError); ok {
		return strings.Contains(strings.ToLower(string(exitErr.Stderr)), "no such")
	}
	return false
}

// mapStatus maps `docker inspect .State.Status` to a ModuleStatus. The running
// states map to RUNNING; everything else (created/exited/dead/paused) maps to
// STOPPED — the module is installed but not actively serving.
func mapStatus(dockerStatus string) fabricpb.ModuleStatus {
	switch dockerStatus {
	case "running", "restarting":
		return fabricpb.ModuleStatus_MODULE_STATUS_RUNNING
	case "":
		return fabricpb.ModuleStatus_MODULE_STATUS_UNSPECIFIED
	default:
		return fabricpb.ModuleStatus_MODULE_STATUS_STOPPED
	}
}

// mapHealth maps the container state + optional healthcheck to a ModuleHealth.
// When a container declares a healthcheck, its result is authoritative
// (healthy/unhealthy/starting). Without one, health is inferred from the run
// state: a running container is HEALTHY, a stopped one UNHEALTHY.
func mapHealth(dockerStatus, healthStatus string) fabricpb.ModuleHealth {
	switch healthStatus {
	case "healthy":
		return fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY
	case "starting":
		return fabricpb.ModuleHealth_MODULE_HEALTH_STARTING
	case "unhealthy":
		return fabricpb.ModuleHealth_MODULE_HEALTH_UNHEALTHY
	}
	// No healthcheck: infer from run state.
	switch dockerStatus {
	case "running":
		return fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY
	case "restarting":
		return fabricpb.ModuleHealth_MODULE_HEALTH_STARTING
	case "":
		return fabricpb.ModuleHealth_MODULE_HEALTH_UNSPECIFIED
	default:
		return fabricpb.ModuleHealth_MODULE_HEALTH_UNHEALTHY
	}
}
