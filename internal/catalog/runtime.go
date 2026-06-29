package catalog

import "os/exec"

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
}

// defaultRuntimeProbes wires the probes to the real host.
func defaultRuntimeProbes() runtimeProbes {
	return runtimeProbes{
		lookPath: func(bin string) bool {
			_, err := exec.LookPath(bin)
			return err == nil
		},
		podmanComposeSubcmd: hostPodmanComposeSubcmd,
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

// SelectContainerRuntime resolves the container runtime to drive module
// containers, preferring rootless podman over docker (#348). It uses the real
// host probes.
func SelectContainerRuntime() ContainerRuntime {
	return selectContainerRuntime(defaultRuntimeProbes())
}

// selectContainerRuntime is the pure core (probes injected) so the selection
// policy is table-testable without podman/docker installed.
//
// Policy, in order:
//  1. podman present + `podman compose` subcommand usable -> podman with the
//     "compose" prefix (rootless).
//  2. podman present + `podman-compose` binary present     -> podman-compose
//     (rootless), empty compose prefix.
//  3. podman present, no compose front-end                 -> docker (podman
//     alone cannot run a compose file; fall back rather than fail).
//  4. podman absent                                        -> docker.
//
// When neither runtime is present we still return docker: the existing start
// path already surfaces a clear docker-not-found error, and returning a concrete
// runtime keeps callers simple. Selection never fails.
func selectContainerRuntime(p runtimeProbes) ContainerRuntime {
	docker := ContainerRuntime{EngineBin: "docker", Bin: "docker", ComposePrefix: []string{"compose"}}

	if !p.lookPath("podman") {
		return docker
	}
	if p.podmanComposeSubcmd() {
		return ContainerRuntime{EngineBin: "podman", Bin: "podman", ComposePrefix: []string{"compose"}, Rootless: true}
	}
	if p.lookPath("podman-compose") {
		// Compose runs via the podman-compose wrapper, but engine sub-commands
		// (inspect/rm) must still go to the podman CLI itself.
		return ContainerRuntime{EngineBin: "podman", Bin: "podman-compose", ComposePrefix: nil, Rootless: true}
	}
	// podman present but no compose front-end: docker can still drive the compose
	// file, so prefer it over failing.
	return docker
}
