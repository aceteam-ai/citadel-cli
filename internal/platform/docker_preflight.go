package platform

import (
	"fmt"
	"os/exec"
	"strings"
)

// DockerHealth is the outcome of a lightweight docker/podman CLI + daemon
// preflight check (citadel #767). It exists so a missing/unusable engine
// produces a clear, actionable diagnosis instead of a raw
// `exec: "docker": executable file not found in $PATH`-style error bubbling
// out of whatever compose-up call happened to trip over it first.
type DockerHealth struct {
	// OK is true when the engine binary is on PATH and its daemon answered.
	OK bool
	// Code classifies the failure for callers that want to branch on it
	// programmatically (e.g. the agentDoctor JSON payload). Empty when OK.
	Code string // "cli_missing" | "daemon_unreachable"
	// Message is a one-line, human-readable diagnosis. Never the raw exec
	// error string.
	Message string
	// Hint is a platform-specific suggested remediation.
	Hint string
}

// Error renders the health check as a single-line message ("" when OK), so a
// DockerHealth can be turned into an error with fmt.Errorf("%s", h) or by
// calling EnsureDockerUsable.
func (h DockerHealth) String() string {
	if h.OK {
		return ""
	}
	if h.Hint == "" {
		return h.Message
	}
	return h.Message + " " + h.Hint
}

// dockerPreflightProbes are the host probes CheckDockerUsable depends on. They
// are an injectable seam so the detection + message logic is unit-testable
// without a real docker/podman/colima/Homebrew install (mirrors
// internal/catalog/runtime.go's runtimeProbes pattern).
type dockerPreflightProbes struct {
	// lookPath reports whether bin resolves on PATH.
	lookPath func(bin string) bool
	// daemonInfo runs a cheap, side-effect-free daemon reachability probe
	// (e.g. `docker info`) and returns its combined output plus any error.
	daemonInfo func(bin string) (output string, err error)
	// colimaRunning reports whether a colima VM appears to be running.
	colimaRunning func() bool
	// dockerDesktopRunning reports whether Docker Desktop appears to be running.
	dockerDesktopRunning func() bool
	// brewFormulaInstalled reports whether `brew list <bin>` succeeds, i.e. the
	// formula is installed even if not linked onto PATH.
	brewFormulaInstalled func(bin string) bool
	// haveBrew reports whether the `brew` binary itself is available.
	haveBrew func() bool
	// goos is the target OS ("darwin"/"linux"/"windows"), injected so the
	// hint text is testable for every platform from any host.
	goos string
}

func defaultDockerPreflightProbes() dockerPreflightProbes {
	return dockerPreflightProbes{
		lookPath: func(bin string) bool {
			_, err := exec.LookPath(bin)
			return err == nil
		},
		daemonInfo: func(bin string) (string, error) {
			out, err := exec.Command(bin, "info").CombinedOutput()
			return string(out), err
		},
		colimaRunning: func() bool {
			return exec.Command("pgrep", "-x", "colima").Run() == nil
		},
		dockerDesktopRunning: func() bool {
			return exec.Command("pgrep", "-f", "Docker Desktop").Run() == nil
		},
		brewFormulaInstalled: func(bin string) bool {
			return exec.Command("brew", "list", bin).Run() == nil
		},
		haveBrew: func() bool {
			_, err := exec.LookPath("brew")
			return err == nil
		},
		goos: OS(),
	}
}

// CheckDockerUsable runs the preflight check against bin (typically "docker",
// or the resolved podman engine binary for callers driving podman). An empty
// bin defaults to "docker".
func CheckDockerUsable(bin string) DockerHealth {
	return checkDockerUsable(bin, defaultDockerPreflightProbes())
}

// EnsureDockerUsable is the convenience error form of CheckDockerUsable: nil
// when usable, otherwise an error carrying the friendly diagnosis + hint.
// Callers that currently exec docker/podman directly (or via docker compose)
// should call this first so an unusable engine fails fast with an actionable
// message instead of surfacing whatever raw exec error the first subprocess
// call happens to hit.
func EnsureDockerUsable(bin string) error {
	h := CheckDockerUsable(bin)
	if h.OK {
		return nil
	}
	return fmt.Errorf("%s", h.String())
}

func checkDockerUsable(bin string, p dockerPreflightProbes) DockerHealth {
	if bin == "" {
		bin = "docker"
	}
	if !p.lookPath(bin) {
		return DockerHealth{
			OK:      false,
			Code:    "cli_missing",
			Message: fmt.Sprintf("%s CLI not found on PATH.", bin),
			Hint:    cliMissingHint(bin, p),
		}
	}
	if out, err := p.daemonInfo(bin); err != nil {
		return DockerHealth{
			OK:      false,
			Code:    "daemon_unreachable",
			Message: fmt.Sprintf("%s daemon is not reachable.", bin),
			Hint:    daemonUnreachableHint(bin, out, p),
		}
	}
	return DockerHealth{OK: true}
}

// cliMissingHint suggests a remediation for a missing engine CLI, per
// platform. On macOS it specifically distinguishes "runtime is running but the
// CLI is unlinked" (the exact case in citadel #767 -- colima healthy, docker
// installed via Homebrew but blocked from linking) from "nothing is
// installed at all".
func cliMissingHint(bin string, p dockerPreflightProbes) string {
	switch p.goos {
	case "darwin":
		runtimeRunning := p.colimaRunning() || p.dockerDesktopRunning()
		formulaInstalled := p.haveBrew() && p.brewFormulaInstalled(bin)
		switch {
		case formulaInstalled && runtimeRunning:
			return fmt.Sprintf("A container runtime (colima/Docker Desktop) is running, but the %s CLI is installed via Homebrew and not linked onto PATH — run 'brew link --overwrite %s'.", bin, bin)
		case formulaInstalled:
			return fmt.Sprintf("Installed via Homebrew but not linked onto PATH — run 'brew link --overwrite %s'.", bin)
		case runtimeRunning:
			return fmt.Sprintf("A container runtime (colima/Docker Desktop) is running, but the %s CLI is missing — run 'brew install %s'.", bin, bin)
		default:
			return fmt.Sprintf("Install it with 'brew install %s' plus a runtime (colima or Docker Desktop), then re-run.", bin)
		}
	case "linux":
		return "Install it with 'sudo citadel init --provision', or see https://docs.docker.com/engine/install/."
	case "windows":
		return "Install it with 'winget install Docker.DockerDesktop', then restart your shell."
	default:
		return fmt.Sprintf("Install %s and ensure it is on PATH.", bin)
	}
}

// daemonUnreachableHint suggests a remediation for a present-but-unreachable
// engine daemon, per platform.
func daemonUnreachableHint(bin, output string, p dockerPreflightProbes) string {
	trimmed := strings.TrimSpace(output)
	switch p.goos {
	case "darwin":
		if p.colimaRunning() {
			return "colima appears to be running but the daemon isn't responding yet — try 'colima restart'."
		}
		return "Start Docker Desktop (or run 'colima start') and try again."
	case "linux":
		hint := "Start the daemon with 'sudo systemctl start docker' (see 'systemctl status docker' for details)."
		if strings.Contains(trimmed, "permission denied") {
			hint = "Permission denied talking to the daemon — log out and back in (or run 'exec su -l $USER') to pick up docker group membership."
		}
		return hint
	case "windows":
		return "Start Docker Desktop and wait for it to report \"running\", then try again."
	default:
		return "Ensure the daemon is running and reachable."
	}
}
