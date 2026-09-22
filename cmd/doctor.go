// cmd/doctor.go
//
// `citadel doctor` (alias `dr`) is a thin, scriptable wrapper over EXISTING
// health checks -- it deliberately does not reimplement detection logic.
// Redo of the closed external-contributor PR #791 (citadel-cli#790), which
// failed to compile because it referenced worker.WorkerSnapshot without
// importing the worker package.
package cmd

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/service"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
	"github.com/spf13/cobra"
)

// doctorReport is the rendered result of `citadel doctor`. It wires together
// independently-owned checks rather than reimplementing their detection:
//
//  1. platform.CheckDockerUsable -- the engine/daemon preflight (citadel
//     #767). This is the CLI-runnable signal: it works from a bare terminal
//     with no other citadel process required.
//  2. platform.CheckRootlessCgroupDelegation and the Podman CDI probe -- the
//     enforcement checks that keep rootless limits and GPU access fail-closed.
//  3. agentDoctor (cmd/agent_tools.go) -- the job-routing / worker-health
//     diagnosis normally served at /agent/doctor by a LIVE `citadel work` /
//     `citadel up` process. A standalone `citadel doctor` invocation has no
//     such process to introspect, so it feeds agentDoctor a zero-value
//     worker.WorkerSnapshot{} -- the only snapshot a CLI-only invocation has
//     available (there is no on-disk WorkerSnapshot marker to load; compare
//     internal/heartbeat/marker.go, which persists heartbeat freshness but
//     not worker routing state). agentDoctor tolerates a zero-value snapshot
//     without crashing (every field is a plain string/bool/pointer and is
//     read via valueOrEmpty / nil-checked formatters), so this renders
//     cleanly -- it just always reports "no live worker" rather than real
//     routing health. That section is therefore informational only here and
//     does NOT affect the exit code; see (doctorReport).ok.
type doctorReport struct {
	dockerHealth platform.DockerHealth
	doctor       map[string]any
	cgroupHealth platform.CgroupDelegationHealth
	gpuHealth    gpuProbeHealth
	// dockerOptional is true on platforms where a working container engine is
	// not required for a healthy node — darwin (citadel-cli#1042): Docker
	// Desktop is optional on a Mac, and a Mac cannot run the CUDA engines that
	// need it anyway (see services.GetAvailableServices' GOOS filter). When set,
	// an unusable engine is a WARN, not a FAIL, and does not fail the exit code.
	dockerOptional bool
	// bindWarnings lists embedded engines (from the node manifest) that will be
	// published on ALL network interfaces with no auth (aceteam-ai/citadel-cli#1023
	// — an opt-in `bind: all`, or an engine defaulting to all-interfaces like
	// ollama). Informational only: it does NOT affect the exit code, matching the
	// posture of the job-routing section (a deliberate `bind: all` is a valid
	// operator choice, not a failure).
	bindWarnings        []string
	serviceExecWarnings []string
}

// ok reports whether doctor found a problem worth a non-zero exit. Engine
// usability and applicable rootless-limit/CDI checks are actionable from a
// standalone invocation. Job-routing checks inside r.doctor essentially
// always read "unhealthy" here (no live worker to inspect), so they remain
// informational and do not affect the result.
func (r doctorReport) ok() bool {
	engineOK := r.dockerHealth.OK || r.dockerOptional
	delegationOK := !r.cgroupHealth.Applicable || r.cgroupHealth.OK
	gpuOK := !r.gpuHealth.Applicable || r.gpuHealth.OK
	return engineOK && delegationOK && gpuOK
}

func (r doctorReport) problem() string {
	if !r.dockerHealth.OK && !r.dockerOptional {
		return r.dockerHealth.String()
	}
	if r.cgroupHealth.Applicable && !r.cgroupHealth.OK {
		return r.cgroupHealth.String()
	}
	if r.gpuHealth.Applicable && !r.gpuHealth.OK {
		return r.gpuHealth.Message
	}
	return "unknown problem"
}

type gpuProbeHealth struct {
	Applicable bool
	OK         bool
	Message    string
}

const doctorCUDAProbeImage = "docker.io/nvidia/cuda:12.4.0-base-ubuntu22.04"

type doctorCommandRunner func(context.Context, string, ...string) ([]byte, error)

func runPodmanGPUProbe(rt catalog.ContainerRuntime, lookPath func(string) (string, error), run doctorCommandRunner) gpuProbeHealth {
	if rt.EngineBin != "podman" || !rt.Rootless || !platform.IsLinux() {
		return gpuProbeHealth{Message: "NVIDIA CDI probe is only applicable to rootless Podman on Linux"}
	}
	if _, err := lookPath("nvidia-smi"); err != nil {
		return gpuProbeHealth{Message: "no host NVIDIA GPU detected; CDI probe not applicable"}
	}
	gpuArgs, err := rt.GPUArgs("all")
	if err != nil {
		return gpuProbeHealth{Applicable: true, Message: err.Error()}
	}
	args := []string{"run", "--rm", "--pull=never"}
	args = append(args, gpuArgs...)
	args = append(args, "--security-opt=label=disable", doctorCUDAProbeImage, "nvidia-smi", "-L")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := run(ctx, rt.EngineBin, args...)
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return gpuProbeHealth{Applicable: true, Message: "Podman NVIDIA CDI probe failed: " + detail}
	}
	detail := strings.TrimSpace(string(out))
	if detail == "" {
		detail = "CUDA device is visible in the container"
	}
	return gpuProbeHealth{Applicable: true, OK: true, Message: detail}
}

// runDoctorChecks gathers the checks doctorReport wires together. Split out
// from doctorRunE so tests can exercise rendering/exit-code logic against a
// hand-built doctorReport without touching a real docker/podman install.
func runDoctorChecks() doctorReport {
	return runDoctorChecksFor(platform.IsDarwin())
}

// runDoctorChecksFor is the seam runDoctorChecks wraps: isDarwin is passed in so
// a test can pin that the darwin branch (Docker optional) is wired to
// platform.IsDarwin() without needing to run on a Mac. runDoctorChecks resolves
// the real value; a hand-built doctorReport in a test would bypass this wiring.
func runDoctorChecksFor(isDarwin bool) doctorReport {
	rt := catalog.SelectContainerRuntime()
	cgroupHealth := platform.CgroupDelegationHealth{OK: true, Message: "not applicable to the selected runtime"}
	if rt.EngineBin == "podman" && rt.Rootless && platform.IsLinux() {
		cgroupHealth = platform.CheckRootlessCgroupDelegation([]string{"cpu", "memory", "pids"})
	}
	gpuHealth := runPodmanGPUProbe(rt, exec.LookPath, func(ctx context.Context, binary string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, binary, args...).CombinedOutput()
	})
	return doctorReport{
		dockerHealth:        platform.CheckDockerUsable(rt.EngineBin),
		doctor:              agentDoctor(worker.WorkerSnapshot{}),
		cgroupHealth:        cgroupHealth,
		gpuHealth:           gpuHealth,
		dockerOptional:      isDarwin,
		bindWarnings:        engineBindExposureWarnings(),
		serviceExecWarnings: service.EphemeralManagedExecStarts(),
	}
}

// renderDoctorReport prints a human-readable report to w.
func renderDoctorReport(w io.Writer, r doctorReport) {
	headerColor.Fprintln(w, "--- 🩺 Citadel Doctor ---")

	headerColor.Fprintln(w, "\nDOCKER / ENGINE")
	switch {
	case r.dockerHealth.OK:
		fmt.Fprintf(w, "  %s docker/engine usable\n", goodColor.Sprint("[OK]"))
	case r.dockerOptional:
		// Docker Desktop is optional on macOS (citadel-cli#1042); an unusable
		// engine is not a failure there.
		fmt.Fprintf(w, "  %s %s\n", warnColor.Sprint("[WARN]"), r.dockerHealth.String())
		fmt.Fprintln(w, faintColor.Sprint("  (Docker Desktop is optional on macOS; install it only to run container-based services)"))
	default:
		fmt.Fprintf(w, "  %s %s\n", badColor.Sprint("[FAIL]"), r.dockerHealth.String())
	}

	headerColor.Fprintln(w, "\nROOTLESS RESOURCE LIMITS")
	if !r.cgroupHealth.Applicable {
		fmt.Fprintf(w, "  %s %s\n", faintColor.Sprint("[N/A]"), r.cgroupHealth.String())
	} else if r.cgroupHealth.OK {
		fmt.Fprintf(w, "  %s delegated controllers: %s\n", goodColor.Sprint("[OK]"), strings.Join(r.cgroupHealth.Controllers, ", "))
	} else {
		fmt.Fprintf(w, "  %s %s\n", badColor.Sprint("[FAIL]"), r.cgroupHealth.String())
		if r.cgroupHealth.Hint != "" {
			fmt.Fprintf(w, "       %s\n", r.cgroupHealth.Hint)
		}
	}

	headerColor.Fprintln(w, "\nPODMAN NVIDIA CDI")
	if !r.gpuHealth.Applicable {
		fmt.Fprintf(w, "  %s %s\n", faintColor.Sprint("[N/A]"), r.gpuHealth.Message)
	} else if r.gpuHealth.OK {
		fmt.Fprintf(w, "  %s %s\n", goodColor.Sprint("[OK]"), r.gpuHealth.Message)
	} else {
		fmt.Fprintf(w, "  %s %s\n", badColor.Sprint("[FAIL]"), r.gpuHealth.Message)
	}

	headerColor.Fprintln(w, "\nJOB ROUTING / WORKER HEALTH")
	fmt.Fprintln(w, faintColor.Sprint("  (informational: reflects a standalone check with no live 'citadel work'/'citadel up' process attached; does not affect exit code)"))
	if checks, ok := r.doctor["checks"].([]map[string]any); ok {
		for _, c := range checks {
			name, _ := c["name"].(string)
			// agentDoctor (cmd/agent_tools.go) runs its own
			// platform.CheckDockerUsable probe and folds the result into this
			// same "checks" slice as "docker_usable". The DOCKER / ENGINE
			// section above already renders that exact signal from
			// r.dockerHealth, so printing it again here would show the docker
			// check twice per `citadel doctor` run (citadel-cli#803). Skip it
			// -- agentDoctor's own return shape is untouched; this is
			// display-only de-dup for the `doctor` command's rendering.
			if name == "docker_usable" {
				continue
			}
			checkOK, _ := c["ok"].(bool)
			detail, _ := c["detail"].(string)
			status := goodColor.Sprint("[OK]")
			if !checkOK {
				status = warnColor.Sprint("[WARN]")
			}
			fmt.Fprintf(w, "  %s %s: %s\n", status, name, detail)
		}
	}
	if diagnosis, ok := r.doctor["diagnosis"].(string); ok && diagnosis != "" {
		fmt.Fprintf(w, "  %s %s\n", labelColor.Sprint("Diagnosis:"), diagnosis)
	}

	headerColor.Fprintln(w, "\nENGINE NETWORK EXPOSURE")
	fmt.Fprintln(w, faintColor.Sprint("  (informational: an engine on all interfaces has no auth of its own; does not affect exit code)"))
	if len(r.bindWarnings) == 0 {
		fmt.Fprintf(w, "  %s no embedded engine is published on all interfaces\n", goodColor.Sprint("[OK]"))
	} else {
		for _, warning := range r.bindWarnings {
			fmt.Fprintf(w, "  %s %s\n", warnColor.Sprint("[WARN]"), warning)
		}
	}

	headerColor.Fprintln(w, "\nSERVICE EXECUTABLE")
	if len(r.serviceExecWarnings) == 0 {
		fmt.Fprintf(w, "  %s no Citadel service ExecStart points at ephemeral storage\n", goodColor.Sprint("[OK]"))
	} else {
		for _, warning := range r.serviceExecWarnings {
			fmt.Fprintf(w, "  %s %s\n", warnColor.Sprint("[WARN]"), warning)
		}
	}

	fmt.Fprintln(w)
	if r.ok() {
		goodColor.Fprintln(w, "Overall: OK")
	} else {
		badColor.Fprintln(w, "Overall: PROBLEMS DETECTED")
	}
}

var doctorCmd = &cobra.Command{
	Use:     "doctor",
	Aliases: []string{"dr"},
	Short:   "Diagnose engine, Podman isolation, GPU, and job-routing health",
	Long: `citadel doctor runs a quick, scriptable health check by wiring together
existing diagnostics rather than reimplementing detection logic:

  - Docker/engine usability (the same preflight used before starting a
    docker-based service): is the CLI on PATH, is the daemon reachable, and
    if not, what's the platform-specific fix?
  - Job-routing / worker health (the same diagnosis served by a live
    'citadel work'/'citadel up' at /agent/doctor): identity resolution,
    per-node stream subscription, and recent poll activity. Since a
    standalone 'citadel doctor' has no live worker to inspect, this section
    is shown for context only and does not affect the exit code.

Exits non-zero if the engine preflight, required rootless cgroup delegation,
or an applicable Podman NVIDIA CDI probe fails, so it can be used in scripts
and health checks.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		report := runDoctorChecks()
		renderDoctorReport(cmd.OutOrStdout(), report)
		if !report.ok() {
			return fmt.Errorf("citadel doctor found a problem: %s", report.problem())
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}
