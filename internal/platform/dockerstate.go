// internal/platform/dockerstate.go
//
// Diagnoses the container-engine CLI/daemon pair a start path is about to exec,
// so a missing "docker" (or "podman") on PATH surfaces as a clear, actionable
// message instead of the raw exec.LookPath error citadel-cli#767 reported:
//
//	[error] Failed to start ollama: exec: "docker": executable file not found in $PATH
//
// The real-world trigger for #767: docker was installed via Homebrew but not
// linked (`brew link` blocked by a completion-file conflict), while a colima
// daemon was happily running -- the CLI was missing, but a runtime was not.
// That distinction (CLI missing vs. daemon unreachable vs. CLI missing WITH a
// runtime evidently running) is what EngineDiagnosis exists to make explicit,
// since each state has a different fix.
//
// Mirrors internal/catalog/runtime.go's shape on purpose: an injectable probe
// struct plus a pure decision function (diagnoseEngine), so the classification
// is table-testable without a real docker/podman/colima install. It lives in
// internal/platform rather than internal/catalog to avoid a package cycle
// (catalog depends on platform, not the reverse); callers that already resolved
// a runtime via catalog.SelectContainerRuntime() pass its EngineBin in here
// rather than this file importing catalog.
package platform

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// daemonInfoTimeout bounds "<bin> info" so a wedged/unresponsive daemon (a
// stale colima VM, a hung Docker Desktop) cannot turn every caller of
// DiagnoseEngine -- citadel status, citadel doctor, and every docker-based
// service start -- into an indefinite hang. A var (not a const) so a test can
// shrink it.
var daemonInfoTimeout = 5 * time.Second

// EngineDiagnosis is the result of probing a container engine CLI/daemon pair.
// Bin is the binary a start path was about to exec -- almost always "docker",
// or "podman" on a node where catalog.SelectContainerRuntime prefers rootless
// podman.
type EngineDiagnosis struct {
	Bin string

	// CLIFound reports whether Bin resolved on PATH.
	CLIFound bool
	CLIPath  string

	// DaemonReachable is meaningful only when CLIFound is true: whether
	// "<bin> info" succeeded. DaemonErr carries its failure text.
	DaemonReachable bool
	DaemonErr       string

	// RuntimeSocket is the path of a live-looking runtime socket found on disk
	// when the CLI itself could not be resolved -- evidence that a runtime
	// (colima, Docker Desktop) is running even though its CLI is not on PATH.
	// Empty when no such socket was found, or when CLIFound (the field is only
	// consulted in the CLI-missing case).
	RuntimeSocket string

	// HomebrewKegPath is set when Homebrew has Bin installed as a formula but
	// its keg is not linked into PATH (`brew link` skipped or blocked) --
	// exactly the citadel-cli#767 trigger. Darwin-only; empty elsewhere.
	HomebrewKegPath string
}

// Healthy reports whether the engine is usable: CLI on PATH and its daemon
// answered.
func (d EngineDiagnosis) Healthy() bool {
	return d.CLIFound && d.DaemonReachable
}

// Diagnose returns a single-line, human-readable explanation of the problem,
// naming what depends on it (e.g. "ollama cannot start") when context is
// non-empty. Callers should surface THIS instead of a raw exec/compose error --
// replacing that raw string is the point of citadel-cli#767. Returns "" when
// Healthy().
func (d EngineDiagnosis) Diagnose(context string) string {
	if d.Healthy() {
		return ""
	}
	suffix := ""
	if context != "" {
		suffix = " — " + context
	}
	if !d.CLIFound {
		msg := fmt.Sprintf("%s CLI not found on PATH%s", d.Bin, suffix)
		switch {
		case d.HomebrewKegPath != "":
			msg += fmt.Sprintf(" (Homebrew has %s installed but not linked: %s)", d.Bin, d.HomebrewKegPath)
		case d.RuntimeSocket != "":
			// The socket proves a socket, not a healthy daemon -- phrase this
			// conservatively rather than claiming the runtime is confirmed good.
			msg += fmt.Sprintf(" (a container runtime appears to be running: found %s)", d.RuntimeSocket)
		}
		return msg
	}
	return fmt.Sprintf("%s daemon is unreachable%s: %s", d.Bin, suffix, d.DaemonErr)
}

// Remediate returns platform-appropriate remediation lines for goos (pass
// platform.OS()). It only ever suggests commands -- citadel has no established
// consent pattern for auto-running system-modifying commands outside `citadel
// init`'s provisioning prompts, and #767 asks for a doctor'd fix, not an
// unattended one. Empty when Healthy().
func (d EngineDiagnosis) Remediate(goos string) []string {
	if d.Healthy() {
		return nil
	}
	if !d.CLIFound {
		if d.HomebrewKegPath != "" {
			return []string{fmt.Sprintf("brew link --overwrite %s", d.Bin)}
		}
		switch goos {
		case "darwin":
			return []string{
				fmt.Sprintf("brew install %s", d.Bin),
				"Install a container runtime: colima (brew install colima && colima start) or Docker Desktop (brew install --cask docker)",
			}
		case "windows":
			return []string{"winget install Docker.DockerDesktop"}
		default: // linux and anything unrecognized
			return []string{
				"Install docker with your package manager (e.g. `curl -fsSL https://get.docker.com | sh`)",
				"then start it: sudo systemctl start docker",
			}
		}
	}
	// CLI found, daemon unreachable.
	switch goos {
	case "darwin":
		return []string{"Start your container runtime: `colima start` (or open Docker Desktop)"}
	case "windows":
		return []string{"Start Docker Desktop"}
	default:
		return []string{"sudo systemctl start docker  (check status: systemctl status docker)"}
	}
}

// engineProbes are the host probes diagnoseEngine depends on -- an injectable
// seam so the classification is unit-testable without a real docker/podman/
// colima install, mirroring internal/catalog's runtimeProbes.
type engineProbes struct {
	// lookPath mirrors exec.LookPath, returning only what the classifier needs.
	lookPath func(bin string) (path string, found bool)
	// daemonInfo runs "<bin> info" (or equivalent) and reports its error.
	daemonInfo func(bin string) error
	// liveSocket returns the path of the first live-looking runtime socket
	// found on disk, or "" if none.
	liveSocket func() string
	// brewKeg reports whether Homebrew has formula installed as an unlinked
	// keg, and its path.
	brewKeg func(formula string) (path string, found bool)
}

// defaultEngineProbes wires engineProbes to the real host.
func defaultEngineProbes() engineProbes {
	return engineProbes{
		lookPath: func(bin string) (string, bool) {
			path, err := exec.LookPath(bin)
			return path, err == nil
		},
		daemonInfo: func(bin string) error {
			ctx, cancel := context.WithTimeout(context.Background(), daemonInfoTimeout)
			defer cancel()
			return exec.CommandContext(ctx, bin, "info").Run()
		},
		liveSocket: hostLiveRuntimeSocket,
		brewKeg:    hostBrewKeg,
	}
}

// DiagnoseEngine probes bin -- the container engine CLI a start path is about
// to exec -- against the real host and classifies its state.
func DiagnoseEngine(bin string) EngineDiagnosis {
	return diagnoseEngine(bin, defaultEngineProbes())
}

// diagnoseEngine is the pure core (probes injected) so the classification is
// table-testable without touching the real host.
func diagnoseEngine(bin string, p engineProbes) EngineDiagnosis {
	d := EngineDiagnosis{Bin: bin}
	if path, ok := p.lookPath(bin); ok {
		d.CLIFound = true
		d.CLIPath = path
		if err := p.daemonInfo(bin); err != nil {
			d.DaemonErr = err.Error()
		} else {
			d.DaemonReachable = true
		}
		return d
	}
	// CLI missing: look for evidence explaining WHY, so the message and
	// remediation can be specific rather than a generic "install docker".
	d.RuntimeSocket = p.liveSocket()
	if kegPath, ok := p.brewKeg(bin); ok {
		d.HomebrewKegPath = kegPath
	}
	return d
}

// hostLiveRuntimeSocket checks the well-known socket paths a colima or Docker
// Desktop runtime binds, in a fixed order, and returns the first that exists.
// A socket file existing is not proof the daemon behind it is healthy -- only
// that something started a runtime -- so EngineDiagnosis.Diagnose phrases this
// conservatively ("appears to be running").
func hostLiveRuntimeSocket() string {
	candidates := []string{"/var/run/docker.sock"}
	if home, err := os.UserHomeDir(); err == nil {
		// Docker Desktop (macOS, and Windows via WSL).
		candidates = append(candidates, filepath.Join(home, ".docker", "run", "docker.sock"))
		// colima names its socket per-profile: ~/.colima/<profile>/docker.sock.
		if matches, globErr := filepath.Glob(filepath.Join(home, ".colima", "*", "docker.sock")); globErr == nil {
			candidates = append(candidates, matches...)
		}
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}

// hostBrewKeg reports whether Homebrew has formula installed as a keg but not
// linked into PATH -- the citadel-cli#767 trigger (docker installed via
// Homebrew, `brew link` blocked by a completion-file conflict). Darwin/Homebrew
// only: a keg-only binary lives at "$(brew --prefix)/opt/<formula>/bin/<formula>"
// regardless of whether the top-level PATH symlink ("$(brew --prefix)/bin/<formula>")
// exists, which is exactly the signal this needs. Returns false on any brew
// failure (not installed, brew itself missing) rather than erroring: this is an
// optional diagnostic, never a hard requirement.
func hostBrewKeg(formula string) (string, bool) {
	if OS() != "darwin" {
		return "", false
	}
	prefixOut, err := exec.Command("brew", "--prefix").Output()
	if err != nil {
		return "", false
	}
	kegBin := filepath.Join(strings.TrimSpace(string(prefixOut)), "opt", formula, "bin", formula)
	if info, statErr := os.Stat(kegBin); statErr == nil && !info.IsDir() {
		return kegBin, true
	}
	return "", false
}
