package catalog

import (
	"fmt"
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

// minimalGPUCaps are the capabilities kept for a GPU module even when the
// manifest declares none. cap_drop: ALL is generally safe for NVIDIA-runtime
// containers (GPU access goes through the runtime, not Linux caps), so the
// default GPU cap set is empty; the NVIDIA device reservation in the BASE
// compose is what grants the GPU, and the override never touches it. This slice
// is the documented hook for keeping a known-required cap should a GPU image
// turn out to need one -- today it is empty by design.
var minimalGPUCaps = []string{}

// composeRoot is the minimal shape we parse from a base compose to enumerate its
// service names. A compose may define several services (an app + a db sidecar);
// the hardening override must target each by name.
type composeRoot struct {
	Services map[string]yaml.Node `yaml:"services"`
}

// GenerateHardeningOverride builds a docker-compose override (a second `-f`
// file) that applies manifest-declared least-privilege to EVERY service defined
// in the base compose. It is PURE (string in, string out) and table-tested.
//
// Per compose service it sets:
//   - security_opt: ["no-new-privileges:true"]
//   - cap_drop: ["ALL"] then cap_add: only the manifest-declared capabilities
//     (plus a minimal set for GPU modules, currently empty by design)
//   - read_only: true plus tmpfs: ["/tmp"] and a tmpfs entry per declared
//     writable_path (so a read-only rootfs module can still write where it must)
//   - pids_limit, mem_limit, cpus from sandbox.resources (or conservative
//     defaults)
//   - devices: only the manifest-declared host devices (omitted if none)
//   - network_mode: "host" ONLY if sandbox.host_network is true (never otherwise)
//
// GPU safety: the override deliberately emits NO GPU/device-reservation block.
// The base compose owns the NVIDIA `deploy.resources.reservations.devices`
// reservation; the override only adds hardening keys, so a legit GPU module
// (e.g. the TEI embedding service, #343) still starts. Note that docker-compose
// list-merges sequences, so a base `cap_add` survives this override's
// `cap_drop: ALL` -- the override mechanism cannot subtract a base directive.
// That residual escalation is already covered by the #342 privilege gate
// (cap_add ALL/SYS_ADMIN is Critical and refused without --allow-privileged).
func GenerateHardeningOverride(baseComposeYAML string, manifest *ServiceManifest) (string, error) {
	var root composeRoot
	if err := yaml.Unmarshal([]byte(baseComposeYAML), &root); err != nil {
		return "", fmt.Errorf("failed to parse base compose to enumerate services: %w", err)
	}
	if len(root.Services) == 0 {
		return "", fmt.Errorf("base compose declares no services: cannot generate a hardening override")
	}

	spec := SandboxSpec{}
	gpu := false
	if manifest != nil {
		spec = manifest.Sandbox
		gpu = manifest.Requires.GPU
	}

	// Resolve the per-service hardening once (identical for every service).
	svc := buildServiceHardening(spec, gpu)

	// Stable, deterministic output: sort service names.
	names := make([]string, 0, len(root.Services))
	for name := range root.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	out := map[string]map[string]any{}
	for _, name := range names {
		out[name] = svc
	}

	doc := map[string]any{"services": out}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("failed to marshal hardening override: %w", err)
	}

	header := "# Citadel least-privilege sandbox override (auto-generated).\n" +
		"# Applied to untrusted (Tier 2) modules on top of the base compose via\n" +
		"#   docker compose -f <name>.yml -f <name>.sandbox.yml up\n" +
		"# Drops all Linux capabilities, forbids privilege escalation, makes the\n" +
		"# root filesystem read-only, and caps resources. Edit the module manifest's\n" +
		"# sandbox: block (then reinstall) to declare additional needs.\n"
	return header + string(data), nil
}

// buildServiceHardening returns the per-service hardening map applied to every
// compose service. Pure helper so GenerateHardeningOverride stays small.
func buildServiceHardening(spec SandboxSpec, gpu bool) map[string]any {
	svc := map[string]any{
		"security_opt": []string{"no-new-privileges:true"},
		"cap_drop":     []string{"ALL"},
		"read_only":    true,
	}

	// Capabilities to keep: declared caps, plus the minimal GPU set for GPU
	// modules. cap_add is only emitted when there is something to add (an empty
	// cap_add list is meaningless and noisy).
	caps := normalizeCaps(spec.Capabilities)
	if gpu {
		caps = append(caps, minimalGPUCaps...)
	}
	caps = dedupeStrings(caps)
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
