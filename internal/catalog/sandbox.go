package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Conservative default cgroup limits applied to an untrusted (Tier 2) module
// when its manifest does not declare an override. They are intentionally modest:
// a module declares what it actually needs via the manifest `sandbox.resources`
// block, and anything bigger is an explicit operator decision.
const (
	defaultSandboxCPUs   = "2.0"
	defaultSandboxMemory = "2g"
	defaultSandboxPIDs   = 512
)

// GenerateHardeningOverride builds a docker/podman compose override (a second
// `-f` file) that applies manifest-declared least-privilege to the services of
// the base compose. It is PURE (string in, string out) and table-tested.
//
// Per NON-GPU compose service it sets ONLY the keys the base service does not
// already declare (inject-only-where-absent, so a module's own explicit choice
// such as `read_only: false` is preserved rather than clobbered by the `-f`
// merge):
//   - security_opt: ["no-new-privileges:true"]
//   - cap_drop: ["ALL"] then cap_add: only the manifest-declared capabilities
//   - read_only: true plus tmpfs: ["/tmp"] and a tmpfs entry per declared
//     writable_path (so a read-only rootfs module can still write where it must)
//   - pids_limit, mem_limit, cpus from sandbox.resources (or conservative
//     defaults)
//   - devices: only the manifest-declared host devices (omitted if none)
//   - network_mode: "host" ONLY if sandbox.host_network is true (never otherwise)
//
// GPU EXEMPTION (#348): a service that REQUESTS A GPU (an NVIDIA
// deploy.resources.reservations.devices entry, a `gpus:` shorthand, or
// `runtime: nvidia`) is omitted from the override ENTIRELY. The conservative
// defaults -- a read-only rootfs, dropped capabilities, and especially the 2g
// memory / 2cpu limits -- break inference/embedding services (vLLM, TEI); the
// only safe containment for those is to leave them as the module authored them.
// This per-service exemption is the reason runtime sandboxing of GPU modules was
// deferred to #348. The base compose owns the GPU reservation regardless; the
// override never emits a GPU/deploy block.
//
// Note that compose list-merges sequences, so a base `cap_add` survives this
// override's `cap_drop: ALL` -- the override mechanism cannot subtract a base
// directive. That residual escalation is already covered by the #342 privilege
// gate (cap_add ALL/SYS_ADMIN is Critical and refused without --allow-privileged).
func GenerateHardeningOverride(baseComposeYAML string, manifest *ServiceManifest) (string, error) {
	services, err := decodeComposeServices(baseComposeYAML)
	if err != nil {
		return "", fmt.Errorf("failed to parse base compose to enumerate services: %w", err)
	}
	if len(services) == 0 {
		return "", fmt.Errorf("base compose declares no services: cannot generate a hardening override")
	}

	spec := SandboxSpec{}
	if manifest != nil {
		spec = manifest.Sandbox
	}

	// Stable, deterministic output: sort service names.
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)

	out := map[string]map[string]any{}
	for _, name := range names {
		base := services[name]
		// GPU/inference services are exempt: omit them from the override so the
		// hardening defaults never break them. A POSITIVE GPU signal is required
		// (fail-safe: an ambiguous service is hardened, not exempted).
		if serviceRequestsGPU(base) {
			continue
		}
		svc := buildServiceHardening(spec, base)
		if len(svc) > 0 {
			out[name] = svc
		}
	}

	doc := map[string]any{"services": out}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("failed to marshal hardening override: %w", err)
	}

	header := "# Citadel least-privilege sandbox override (auto-generated).\n" +
		"# Applied to untrusted (Tier 2) modules on top of the base compose via\n" +
		"#   <runtime> compose -f <name>.yml -f <name>.sandbox.yml up\n" +
		"# Drops all Linux capabilities, forbids privilege escalation, makes the\n" +
		"# root filesystem read-only, and caps resources. GPU/inference services are\n" +
		"# exempt (their defaults would break them). Edit the module manifest's\n" +
		"# sandbox: block (then reinstall) to declare additional needs.\n"
	return header + string(data), nil
}

// buildServiceHardening returns the per-service hardening map for one NON-GPU
// service. It injects each hardening key ONLY when the base service does not
// already declare it (inject-only-where-absent), so an explicit base choice is
// preserved. Pure helper so GenerateHardeningOverride stays small.
func buildServiceHardening(spec SandboxSpec, base map[string]any) map[string]any {
	full := buildFullServiceHardening(spec)
	svc := map[string]any{}
	for k, v := range full {
		if _, set := base[k]; set {
			continue // base already declares it -- preserve the module's choice
		}
		svc[k] = v
	}
	return svc
}

// buildFullServiceHardening returns the complete set of hardening keys for a
// non-GPU service before the inject-only-where-absent filter. Splitting this out
// keeps the "what hardening looks like" logic separate from the "what is already
// set" filter.
func buildFullServiceHardening(spec SandboxSpec) map[string]any {
	svc := map[string]any{
		"security_opt": []string{"no-new-privileges:true"},
		"cap_drop":     []string{"ALL"},
		"read_only":    true,
	}

	// Capabilities to keep: only the manifest-declared caps (GPU services are
	// exempt from hardening entirely, so no GPU cap set is needed here). cap_add
	// is only emitted when there is something to add (an empty cap_add list is
	// meaningless and noisy).
	caps := dedupeStrings(normalizeCaps(spec.Capabilities))
	if len(caps) > 0 {
		svc["cap_add"] = caps
	}

	// Read-only rootfs: provide writable tmpfs mounts. /tmp is always writable;
	// declared writable_paths are added (deduped, /tmp not duplicated).
	tmpfs := []string{"/tmp"}
	for _, p := range spec.WritablePaths {
		p = strings.TrimSpace(p)
		if p == "" || p == "/tmp" {
			continue
		}
		tmpfs = append(tmpfs, p)
	}
	svc["tmpfs"] = dedupeStrings(tmpfs)

	// Resource limits: declared or conservative defaults.
	cpus := strings.TrimSpace(spec.Resources.CPU)
	if cpus == "" {
		cpus = defaultSandboxCPUs
	}
	mem := strings.TrimSpace(spec.Resources.Memory)
	if mem == "" {
		mem = defaultSandboxMemory
	}
	pids := spec.Resources.PIDs
	if pids <= 0 {
		pids = defaultSandboxPIDs
	}
	svc["cpus"] = cpus
	svc["mem_limit"] = mem
	svc["pids_limit"] = pids

	// Host devices: only what the manifest declares.
	if devs := dedupeStrings(trimAll(spec.Devices)); len(devs) > 0 {
		svc["devices"] = devs
	}

	// Host networking: opt-in only.
	if spec.HostNetwork {
		svc["network_mode"] = "host"
	}

	return svc
}

// normalizeCaps trims and uppercases declared capability names, dropping empties.
// Names are emitted verbatim (with or without a CAP_ prefix as declared); docker
// accepts both forms.
func normalizeCaps(caps []string) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		out = append(out, c)
	}
	return out
}

func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// --- bind-mount confinement (enforcement on top of the #342 risk-scan warning) ---

// SandboxDataDir returns the per-module sandbox data directory for a module
// installed into servicesDir. Host bind-mounts of an untrusted module must stay
// within this directory unless --allow-privileged is set.
func SandboxDataDir(servicesDir, name string) string {
	return filepath.Join(servicesDir, name+"-data")
}

// SandboxOverridePath returns the path of a module's hardening override
// (<servicesDir>/<name>.sandbox.yml). It is the single source of truth for the
// override filename so every docker-compose start site (across packages) can
// resolve and stat the same file. It does NOT check existence.
func SandboxOverridePath(servicesDir, name string) string {
	return filepath.Join(servicesDir, name+".sandbox.yml")
}

// ExistingSandboxOverride returns SandboxOverridePath if that file exists, else
// "". Start sites append it as a second `-f` when non-empty -- a no-op for every
// non-sandboxed (trusted/pre-sandbox) service.
func ExistingSandboxOverride(servicesDir, name string) string {
	p := SandboxOverridePath(servicesDir, name)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// BindMountViolations returns the host bind-mount paths in a compose that an
// untrusted module is NOT permitted to mount: any host bind-mount whose host
// side resolves outside the per-module sandbox data dir. Named volumes and
// non-path mounts are ignored. It is PURE (compose text + dirs in, slice out)
// and table-tested. Parsing is line-based, consistent with risk.go.
//
// Relative host paths (e.g. "./data") are resolved against servicesDir, the
// directory the compose file is copied into -- matching how docker-compose
// resolves a relative bind path against the compose file's directory.
func BindMountViolations(compose, servicesDir, name string) []string {
	allowedRoot := SandboxDataDir(servicesDir, name)
	var violations []string

	for _, raw := range strings.Split(compose, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		item, ok := listItem(line)
		if !ok {
			continue
		}
		// A bind mount is "HOST:CONTAINER[:opts]"; a named volume is
		// "name:CONTAINER" (left side is not a path).
		parts := strings.SplitN(item, ":", 2)
		if len(parts) < 2 {
			continue
		}
		host := strings.TrimSpace(parts[0])
		if !looksLikeHostPath(host) {
			continue // named volume, not a host bind mount
		}

		resolved := resolveHostBindPath(host, servicesDir)
		if !withinDir(allowedRoot, resolved) {
			violations = append(violations, host)
		}
	}
	return violations
}

// resolveHostBindPath resolves a compose host bind path for confinement checks.
// Absolute paths are cleaned as-is; "~"/"~/..." is left as a literal absolute-ish
// marker (a home mount is always outside the sandbox dir); relative paths are
// resolved against servicesDir (the compose file's directory).
func resolveHostBindPath(host, servicesDir string) string {
	switch {
	case host == "~" || strings.HasPrefix(host, "~/"):
		// Home shortcut: always outside the per-module sandbox dir. Return a
		// sentinel that cannot be within allowedRoot.
		return filepath.Clean("/__home__" + strings.TrimPrefix(host, "~"))
	case filepath.IsAbs(host):
		return filepath.Clean(host)
	default:
		return filepath.Clean(filepath.Join(servicesDir, host))
	}
}

// withinDir reports whether path is dir itself or nested under it. Boundary-aware
// (cleans both, then checks for equality or a "dir/" prefix) so "/a/b-data" is
// not considered within "/a/b".
func withinDir(dir, path string) bool {
	dir = filepath.Clean(dir)
	path = filepath.Clean(path)
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}
