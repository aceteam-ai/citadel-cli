package platform

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestDockerInfoTimeoutBounded pins dockerInfoTimeout to a sane, non-zero
// window. The real probe path (which this constant guards) is not exercised
// by the stubbed tests below, so nothing else would notice it being deleted
// or zeroed -- a wedged daemon must still be bounded, and a bound that's too
// tight would misclassify a merely-slow daemon as unreachable.
func TestDockerInfoTimeoutBounded(t *testing.T) {
	if dockerInfoTimeout <= 0 {
		t.Fatalf("dockerInfoTimeout must be > 0 (unbounded probe can hang citadel doctor), got %v", dockerInfoTimeout)
	}
	if dockerInfoTimeout > 10*time.Second {
		t.Fatalf("dockerInfoTimeout = %v is too generous for a doctor/preflight check", dockerInfoTimeout)
	}
}

func TestCheckDockerUsable_OK(t *testing.T) {
	p := dockerPreflightProbes{
		lookPath:   func(bin string) bool { return true },
		daemonInfo: func(bin string) (string, error) { return "", nil },
		goos:       "linux",
	}
	h := checkDockerUsable("docker", p)
	if !h.OK {
		t.Fatalf("expected OK, got %+v", h)
	}
	if h.String() != "" {
		t.Fatalf("expected empty String() when OK, got %q", h.String())
	}
}

func TestCheckDockerUsable_CLIMissing_macOS_UnlinkedFormula(t *testing.T) {
	// This is the exact citadel #767 repro: colima is running with a healthy
	// daemon, docker was installed via Homebrew but never linked, so "docker"
	// is not on PATH.
	p := dockerPreflightProbes{
		lookPath:             func(bin string) bool { return false },
		daemonInfo:           func(bin string) (string, error) { return "", errors.New("unused") },
		colimaRunning:        func() bool { return true },
		dockerDesktopRunning: func() bool { return false },
		brewFormulaInstalled: func(bin string) bool { return true },
		haveBrew:             func() bool { return true },
		goos:                 "darwin",
	}
	h := checkDockerUsable("docker", p)
	if h.OK {
		t.Fatalf("expected NOT ok")
	}
	if h.Code != "cli_missing" {
		t.Fatalf("expected code cli_missing, got %q", h.Code)
	}
	if strings.Contains(h.String(), "exec:") {
		t.Fatalf("diagnosis must not leak the raw exec error string, got %q", h.String())
	}
	if !strings.Contains(h.Hint, "brew link --overwrite docker") {
		t.Fatalf("expected unlinked-formula hint to suggest brew link, got %q", h.Hint)
	}
}

func TestCheckDockerUsable_CLIMissing_macOS_NothingInstalled(t *testing.T) {
	p := dockerPreflightProbes{
		lookPath:             func(bin string) bool { return false },
		daemonInfo:           func(bin string) (string, error) { return "", errors.New("unused") },
		colimaRunning:        func() bool { return false },
		dockerDesktopRunning: func() bool { return false },
		brewFormulaInstalled: func(bin string) bool { return false },
		haveBrew:             func() bool { return true },
		goos:                 "darwin",
	}
	h := checkDockerUsable("docker", p)
	if h.OK {
		t.Fatalf("expected NOT ok")
	}
	if !strings.Contains(h.Hint, "brew install docker") {
		t.Fatalf("expected fresh-install hint to suggest brew install, got %q", h.Hint)
	}
}

func TestCheckDockerUsable_CLIMissing_Linux(t *testing.T) {
	p := dockerPreflightProbes{
		lookPath:   func(bin string) bool { return false },
		daemonInfo: func(bin string) (string, error) { return "", errors.New("unused") },
		goos:       "linux",
	}
	h := checkDockerUsable("docker", p)
	if h.OK || h.Code != "cli_missing" {
		t.Fatalf("expected cli_missing, got %+v", h)
	}
	if !strings.Contains(h.Hint, "citadel init --provision") {
		t.Fatalf("expected linux hint to mention citadel init --provision, got %q", h.Hint)
	}
}

func TestCheckDockerUsable_DaemonUnreachable(t *testing.T) {
	p := dockerPreflightProbes{
		lookPath: func(bin string) bool { return true },
		daemonInfo: func(bin string) (string, error) {
			return "Cannot connect to the Docker daemon", errors.New("exit status 1")
		},
		colimaRunning: func() bool { return false },
		goos:          "linux",
	}
	h := checkDockerUsable("docker", p)
	if h.OK {
		t.Fatalf("expected NOT ok")
	}
	if h.Code != "daemon_unreachable" {
		t.Fatalf("expected code daemon_unreachable, got %q", h.Code)
	}
	if !strings.Contains(h.Hint, "systemctl start docker") {
		t.Fatalf("expected linux daemon hint to mention systemctl, got %q", h.Hint)
	}
}

func TestCheckDockerUsable_DefaultsBinToDocker(t *testing.T) {
	var seenBin string
	p := dockerPreflightProbes{
		lookPath: func(bin string) bool { seenBin = bin; return true },
		daemonInfo: func(bin string) (string, error) {
			return "", nil
		},
		goos: "linux",
	}
	h := checkDockerUsable("", p)
	if !h.OK {
		t.Fatalf("expected OK, got %+v", h)
	}
	if seenBin != "docker" {
		t.Fatalf("expected empty bin to default to \"docker\", got %q", seenBin)
	}
}

// TestPreflightDockerStart_DaemonUnreachable_WarnsAndProceeds pins the
// coordinator-requested behavior fix: a service-start preflight must NOT
// hard-refuse just because `docker info` failed (including a timeout on a
// slow-but-live daemon) -- before this preflight existed, a slow daemon just
// made `docker compose up` slower, and docker's own error already covers the
// truly-down case. Only cli_missing may refuse.
func TestPreflightDockerStart_DaemonUnreachable_WarnsAndProceeds(t *testing.T) {
	p := dockerPreflightProbes{
		lookPath: func(bin string) bool { return true },
		daemonInfo: func(bin string) (string, error) {
			return "", errors.New("context deadline exceeded")
		},
		colimaRunning: func() bool { return false },
		goos:          "linux",
	}
	refuseErr, warning := preflightDockerStart("docker", p)
	if refuseErr != nil {
		t.Fatalf("daemon_unreachable must NOT refuse the start, got refuseErr=%v", refuseErr)
	}
	if warning == "" {
		t.Fatalf("expected a non-empty warning for daemon_unreachable")
	}
	if strings.Contains(warning, "exec:") {
		t.Fatalf("warning must not leak the raw exec error, got %q", warning)
	}
}

// TestPreflightDockerStart_CLIMissing_Refuses pins the other half: a missing
// CLI (an exec that would fail immediately anyway) is still a hard refusal.
func TestPreflightDockerStart_CLIMissing_Refuses(t *testing.T) {
	p := dockerPreflightProbes{
		lookPath:             func(bin string) bool { return false },
		daemonInfo:           func(bin string) (string, error) { return "", errors.New("unused") },
		colimaRunning:        func() bool { return false },
		dockerDesktopRunning: func() bool { return false },
		brewFormulaInstalled: func(bin string) bool { return false },
		haveBrew:             func() bool { return false },
		goos:                 "linux",
	}
	refuseErr, warning := preflightDockerStart("docker", p)
	if refuseErr == nil {
		t.Fatalf("expected cli_missing to refuse the start")
	}
	if warning != "" {
		t.Fatalf("expected no warning alongside a refusal, got %q", warning)
	}
}

// TestPreflightDockerStart_OK pins the healthy no-op case: no refusal, no
// warning.
func TestPreflightDockerStart_OK(t *testing.T) {
	p := dockerPreflightProbes{
		lookPath:   func(bin string) bool { return true },
		daemonInfo: func(bin string) (string, error) { return "", nil },
		goos:       "linux",
	}
	refuseErr, warning := preflightDockerStart("docker", p)
	if refuseErr != nil || warning != "" {
		t.Fatalf("expected no refusal and no warning on a healthy engine, got refuseErr=%v warning=%q", refuseErr, warning)
	}
}

func TestEnsureDockerUsable_WrapsError(t *testing.T) {
	// EnsureDockerUsable exercises the real (non-stubbed) probes, so this just
	// checks the OK short-circuit and the error-wrapping shape rather than
	// depending on any specific host state.
	err := EnsureDockerUsable("definitely-not-a-real-binary-citadel-767")
	if err == nil {
		t.Fatalf("expected an error for a nonexistent binary")
	}
	if !strings.Contains(err.Error(), "definitely-not-a-real-binary-citadel-767") {
		t.Fatalf("expected error to name the binary, got %q", err.Error())
	}
}
