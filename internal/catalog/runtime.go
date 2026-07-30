package catalog

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// RuntimeOverrideEnv is the environment variable that forces a container
// runtime, bypassing auto-detection. Valid values: "docker", "podman". The
// root command's --runtime flag sets it. An unrecognized value is ignored
// (auto-detection proceeds) and reported via ContainerRuntime.FallbackReason.
const RuntimeOverrideEnv = "CITADEL_CONTAINER_RUNTIME"

// ContainerRuntime describes the container runtime (and its compose front-end)
// that Citadel should drive for module containers. Module containment (#348)
// prefers podman rootless over docker where available: podman is largely
// CLI-compatible with docker, but its compose front-end differs (the `podman
// compose` subcommand vs the separate `podman-compose` binary vs docker's
// `docker compose`). Callers therefore must not hardcode the binary or the
// compose prefix; they resolve a ContainerRuntime once and build commands from
// it.
type ContainerRuntime struct {
	// EngineBin is the engine CLI binary (always "docker" or "podman") used for
	// plain engine sub-commands such as `inspect`, `rm`, `ps`. It is NEVER
	// "podman-compose": that wrapper is a compose front-end, not an engine CLI,
	// and does not accept `inspect`/`rm`. Use EngineBin for engine sub-commands
	// and Bin (+ComposePrefix) for compose invocations.
	EngineBin string
	// Bin is the binary to exec for a COMPOSE invocation (e.g. "docker",
	// "podman", or "podman-compose"). Combined with ComposePrefix it forms the
	// compose command.
	Bin string
	// ComposePrefix is the argument prefix that selects the compose front-end for
	// Bin. For docker / podman-with-subcommand it is ["compose"]; for the separate
	// `podman-compose` binary it is empty (Bin is "podman-compose").
	ComposePrefix []string
	// Rootless reports whether this runtime runs rootless by default (podman).
	// Informational: callers may surface it; it does not change argument
	// construction.
	Rootless bool
	// FallbackReason explains why selection could not honor the preferred
	// runtime and downgraded to another one -- e.g. podman is installed but its
	// API socket is not listening (#636). Empty when the preferred runtime was
	// selected outright. SelectContainerRuntime logs it once per process.
	FallbackReason string
}

// Label returns a short human-readable description of the selected runtime for
// operator-facing logging (e.g. "podman (rootless)" or "docker").
func (rt ContainerRuntime) Label() string {
	name := rt.EngineBin
	if name == "" {
		name = rt.Bin
	}
	if rt.Rootless {
		return name + " (rootless)"
	}
	return name
}

// ComposeArgs returns the full argument list for a compose invocation on this
// runtime: the compose prefix followed by the caller's compose args. The binary
// to exec is rt.Bin.
func (rt ContainerRuntime) ComposeArgs(args ...string) []string {
	out := make([]string, 0, len(rt.ComposePrefix)+len(args))
	out = append(out, rt.ComposePrefix...)
	return append(out, args...)
}

// runtimeProbes are the host probes the runtime selector depends on. They are an
// injectable seam so selectContainerRuntime is unit-testable without podman or
// docker installed.
type runtimeProbes struct {
	// lookPath reports whether a binary is resolvable on PATH (mirrors
	// exec.LookPath, returning only the boolean we need).
	lookPath func(bin string) bool
	// podmanComposeSubcmd reports whether `podman compose` (the built-in compose
	// subcommand) is usable, distinct from the separate `podman-compose` binary.
	podmanComposeSubcmd func() bool
	// podmanSocketLive reports whether podman's Docker-compatible API socket is
	// actually listening. This is NOT implied by podmanComposeSubcmd: `podman
	// compose version` succeeds by delegating to an external provider (usually
	// docker-compose) that is present on PATH, even when the socket it would
	// connect to is dead (#636).
	podmanSocketLive func() bool
	// override returns a forced runtime name ("docker"/"podman"), or "" to
	// auto-detect.
	override func() string
}

// defaultRuntimeProbes wires the probes to the real host.
func defaultRuntimeProbes() runtimeProbes {
	return runtimeProbes{
		lookPath: func(bin string) bool {
			_, err := exec.LookPath(bin)
			return err == nil
		},
		podmanComposeSubcmd: hostPodmanComposeSubcmd,
		podmanSocketLive:    hostPodmanSocketLive,
		override:            func() string { return os.Getenv(RuntimeOverrideEnv) },
	}
}

// hostPodmanComposeSubcmd reports whether `podman compose version` succeeds,
// i.e. the built-in compose subcommand is wired up on this host. A failure (or
// no podman) returns false so the caller falls back to the `podman-compose`
// binary.
func hostPodmanComposeSubcmd() bool {
	if _, err := exec.LookPath("podman"); err != nil {
		return false
	}
	// `podman compose version` is a cheap, side-effect-free probe of the compose
	// provider. It exits non-zero when no provider is configured.
	return exec.Command("podman", "compose", "version").Run() == nil
}

// hostPodmanSocketLive reports whether podman's Docker-compatible API socket is
// listening, which is what `podman compose` needs: it delegates to an external
// compose provider that speaks the Docker API over that socket.
//
// podman itself reports this, so we do not depend on systemd being the thing
// that manages the socket:
//
//	podman info --format {{.Host.RemoteSocket.Exists}}
//
// A node with podman installed but `podman.socket` inactive answers "false" --
// exactly the state that made every module fail with "Cannot connect to the
// Docker daemon at unix:///run/user/1000/podman/podman.sock" (#636).
func hostPodmanSocketLive() bool {
	if _, err := exec.LookPath("podman"); err != nil {
		return false
	}
	out, err := exec.Command("podman", "info", "--format", "{{.Host.RemoteSocket.Exists}}").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// warnOnce guards the fallback warning so repeatedly-called read paths (status,
// resmon, footprint) do not spam it.
var warnOnce sync.Once

// SelectContainerRuntime resolves the container runtime to drive module
// containers, preferring rootless podman over docker (#348). It uses the real
// host probes, and logs once if it had to fall back from the preferred runtime.
func SelectContainerRuntime() ContainerRuntime {
	rt := selectContainerRuntime(defaultRuntimeProbes())
	if rt.FallbackReason != "" {
		warnOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "warning: %s\n", rt.FallbackReason)
		})
	}
	return rt
}

// selectContainerRuntime is the pure core (probes injected) so the selection
// policy is table-testable without podman/docker installed.
//
// Policy, in order:
//  0. An explicit override (--runtime / CITADEL_CONTAINER_RUNTIME) wins outright.
//  1. podman present + `podman compose` usable + podman socket live -> podman
//     with the "compose" prefix (rootless).
//  2. podman present + `podman-compose` binary present -> podman-compose
//     (rootless), empty compose prefix. This wrapper drives the podman CLI
//     directly, so it does NOT need the API socket.
//  3. podman present, no usable compose front-end (or a compose subcommand whose
//     socket is dead) -> docker, with FallbackReason set.
//  4. podman absent -> docker.
//
// When neither runtime is present we still return docker: the existing start
// path already surfaces a clear docker-not-found error, and returning a concrete
// runtime keeps callers simple. Selection never fails.
func selectContainerRuntime(p runtimeProbes) ContainerRuntime {
	docker := ContainerRuntime{EngineBin: "docker", Bin: "docker", ComposePrefix: []string{"compose"}}
	podmanCompose := ContainerRuntime{EngineBin: "podman", Bin: "podman", ComposePrefix: []string{"compose"}, Rootless: true}
	// Compose runs via the podman-compose wrapper, but engine sub-commands
	// (inspect/rm) must still go to the podman CLI itself.
	podmanWrapper := ContainerRuntime{EngineBin: "podman", Bin: "podman-compose", ComposePrefix: nil, Rootless: true}

	var invalidOverride string
	switch strings.ToLower(strings.TrimSpace(p.override())) {
	case "docker":
		return docker
	case "podman":
		// Forced: honor the operator's choice and pick the best available
		// front-end. If neither is usable we still return podman so the failure
		// is loud and attributable, rather than silently running docker.
		if !p.podmanComposeSubcmd() && p.lookPath("podman-compose") {
			return podmanWrapper
		}
		return podmanCompose
	case "":
		// auto-detect
	default:
		invalidOverride = fmt.Sprintf("ignoring unrecognized %s=%q (expected \"docker\" or \"podman\"); auto-detecting instead",
			RuntimeOverrideEnv, p.override())
	}

	withReason := func(rt ContainerRuntime, reason string) ContainerRuntime {
		switch {
		case invalidOverride != "" && reason != "":
			rt.FallbackReason = invalidOverride + "; " + reason
		case invalidOverride != "":
			rt.FallbackReason = invalidOverride
		default:
			rt.FallbackReason = reason
		}
		return rt
	}

	if !p.lookPath("podman") {
		return withReason(docker, "")
	}
	if p.podmanComposeSubcmd() {
		if p.podmanSocketLive() {
			return withReason(podmanCompose, "")
		}
		// `podman compose` exists but its API socket is dead, so every compose
		// call would fail against a socket nobody is listening on. The
		// podman-compose wrapper talks to the podman CLI directly and still
		// works, so prefer it before giving up on podman entirely.
		if p.lookPath("podman-compose") {
			return withReason(podmanWrapper, "")
		}
		return withReason(docker, "podman is preferred on this node but its API socket is not responding "+
			"(start podman.socket, or pass --runtime=podman to force it); falling back to docker")
	}
	if p.lookPath("podman-compose") {
		return withReason(podmanWrapper, "")
	}
	// podman present but no compose front-end: docker can still drive the compose
	// file, so prefer it over failing.
	return withReason(docker, "")
}
