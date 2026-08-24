// cmd/doctor.go
//
// `citadel doctor` (alias `dr`) is a thin, scriptable wrapper over EXISTING
// health checks -- it deliberately does not reimplement detection logic.
// Redo of the closed external-contributor PR #791 (citadel-cli#790), which
// failed to compile because it referenced worker.WorkerSnapshot without
// importing the worker package.
package cmd

import (
	"fmt"
	"io"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
	"github.com/spf13/cobra"
)

// doctorReport is the rendered result of `citadel doctor`. It wires together
// two independently-owned checks rather than reimplementing either:
//
//  1. platform.CheckDockerUsable -- the engine/daemon preflight (citadel
//     #767). This is the CLI-runnable signal: it works from a bare terminal
//     with no other citadel process required, so it is what decides the
//     command's exit code.
//  2. agentDoctor (cmd/agent_tools.go) -- the job-routing / worker-health
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
}

// ok reports whether doctor found a problem worth a non-zero exit. Only the
// docker/engine preflight decides this: it is the one check in this report
// that means something for a standalone invocation. The job-routing checks
// inside r.doctor essentially always read "unhealthy" here (no live worker
// to resolve Headscale identity, subscribe the per-node stream, etc.), which
// would make the exit code fire on every idle node if it were included --
// that is expected standalone state, not a problem to report.
func (r doctorReport) ok() bool {
	return r.dockerHealth.OK
}

// runDoctorChecks gathers the checks doctorReport wires together. Split out
// from doctorRunE so tests can exercise rendering/exit-code logic against a
// hand-built doctorReport without touching a real docker/podman install.
func runDoctorChecks() doctorReport {
	bin := catalog.SelectContainerRuntime().EngineBin
	return doctorReport{
		dockerHealth: platform.CheckDockerUsable(bin),
		doctor:       agentDoctor(worker.WorkerSnapshot{}),
	}
}

// renderDoctorReport prints a human-readable report to w.
func renderDoctorReport(w io.Writer, r doctorReport) {
	headerColor.Fprintln(w, "--- 🩺 Citadel Doctor ---")

	headerColor.Fprintln(w, "\nDOCKER / ENGINE")
	if r.dockerHealth.OK {
		fmt.Fprintf(w, "  %s docker/engine usable\n", goodColor.Sprint("[OK]"))
	} else {
		fmt.Fprintf(w, "  %s %s\n", badColor.Sprint("[FAIL]"), r.dockerHealth.String())
	}

	headerColor.Fprintln(w, "\nJOB ROUTING / WORKER HEALTH")
	fmt.Fprintln(w, faintColor.Sprint("  (informational: reflects a standalone check with no live 'citadel work'/'citadel up' process attached; does not affect exit code)"))
	if checks, ok := r.doctor["checks"].([]map[string]any); ok {
		for _, c := range checks {
			name, _ := c["name"].(string)
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
	Short:   "Diagnose common node problems (docker/engine usability, job-routing health)",
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

Exits non-zero if the docker/engine preflight fails, so it can be used in
scripts and health checks.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		report := runDoctorChecks()
		renderDoctorReport(cmd.OutOrStdout(), report)
		if !report.ok() {
			return fmt.Errorf("citadel doctor found a problem: %s", report.dockerHealth.String())
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}
