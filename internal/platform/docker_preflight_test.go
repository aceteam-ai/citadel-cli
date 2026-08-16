package platform

import (
	"errors"
	"strings"
	"testing"
)

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
