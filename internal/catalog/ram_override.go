// internal/catalog/ram_override.go
//
// Per-service RAM ceiling override for GPU/media services (citadel#831,
// docs/design-resource-isolation.md §2). GenerateHardeningOverride (sandbox.go)
// already proves the delivery mechanism -- a second `-f` compose file injecting
// mem_limit -- but deliberately EXEMPTS any service that requests a GPU (its
// cap-drop/read-only-rootfs/2g-default hardening breaks inference engines).
// That exemption is correct for what it protects against and stays in place;
// what's missing is a NARROWER override -- mem_limit only, no cap changes, no
// read-only rootfs -- for exactly the population GenerateHardeningOverride
// skips. RAM limiting is orthogonal to the GPU-hardening tradeoff: a GPU job
// can still exhaust host RAM (the 2026-08-25 incident that motivated #831 was
// exactly this -- a CPU-offloaded ~19GB text encoder, not a GPU/VRAM issue).
package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// GPURAMOverridePath returns the path of a service's GPU RAM-ceiling override
// (<servicesDir>/<name>.ram.yml). Deliberately a DIFFERENT filename from
// SandboxOverridePath's <name>.sandbox.yml: the two overrides target disjoint
// service populations (Tier-2 untrusted modules minus GPU services, vs GPU
// services only) and can be applied together as separate `-f` files with no
// key collision, but keeping them as separate files means a module manifest's
// own sandbox.resources choice and this ceiling can never silently clobber
// each other in one generated document.
func GPURAMOverridePath(servicesDir, name string) string {
	return filepath.Join(servicesDir, name+".ram.yml")
}

// ExistingGPURAMOverride returns GPURAMOverridePath if that file exists, else
// "". Mirrors ExistingSandboxOverride's SHAPE, but has NO production caller
// today (deliberately -- cmd/service.go's composeFileArgs explicitly does
// NOT wire this in; see its doc comment): a boot-time/`citadel run` start
// site has no per-call access to CITADEL_RESOURCE_ISOLATION, so blindly
// appending whatever `<name>.ram.yml` exists on disk would keep applying a
// stale ceiling after an operator turns the flag back off, silently
// defeating the opt-out. Kept exported and tested as the documented,
// symmetric counterpart to GPURAMOverridePath/GenerateGPUMemoryOverride; a
// future caller MUST thread the flag check through before using it, not
// just call it the way ExistingSandboxOverride's callers do.
func ExistingGPURAMOverride(servicesDir, name string) string {
	p := GPURAMOverridePath(servicesDir, name)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// GenerateGPUMemoryOverride builds a compose override that sets ONLY
// mem_limit on services that request a GPU, so a runaway process inside a
// GPU/media-gen container is killed by ITS OWN cgroup memory.max before the
// host's global OOM killer has to pick a victim across every container on the
// box (the citadel#831 acceptance criterion). It is PURE (string in, string
// out) and mirrors GenerateHardeningOverride's shape:
//
//   - Inject-only-where-absent: a service that already declares its own
//     mem_limit in the base compose (or, orthogonally, gets one from a Tier-2
//     sandbox override generated separately) is left untouched here -- an
//     explicit prior choice is never clobbered.
//   - GPU-gated the same fail-safe direction as GenerateHardeningOverride: a
//     service must BOTH request a GPU (serviceRequestsGPU) AND hostHasGPU be
//     true. An ambiguous/non-GPU service, or a "GPU" service on a host that
//     turns out to have none, gets no override from this function.
//   - Stable, deterministic output (sorted service names) for diffability and
//     table-testing, matching GenerateHardeningOverride.
//
// memLimitBytes is the ceiling to apply to every qualifying service (the
// caller -- internal/jobs' RAM-isolation wiring -- derives one number per
// start via status.RAMBudgetBytes and passes it in here; this function does
// no budget math of its own). Returns "" (not an error) when no service in
// the compose qualifies, so callers can treat an empty result as "nothing to
// write" without a special case.
func GenerateGPUMemoryOverride(baseComposeYAML string, memLimitBytes int64, hostHasGPU bool) (string, error) {
	if memLimitBytes <= 0 {
		return "", fmt.Errorf("memLimitBytes must be positive, got %d", memLimitBytes)
	}

	services, err := decodeComposeServices(baseComposeYAML)
	if err != nil {
		return "", fmt.Errorf("failed to parse base compose to enumerate services: %w", err)
	}
	if len(services) == 0 {
		return "", fmt.Errorf("base compose declares no services: cannot generate a GPU memory override")
	}

	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)

	out := map[string]map[string]any{}
	for _, name := range names {
		base := services[name]
		if !hostHasGPU || !serviceRequestsGPU(base) {
			continue
		}
		if _, set := base["mem_limit"]; set {
			continue // base (or an earlier override) already declares its own choice
		}
		out[name] = map[string]any{"mem_limit": fmt.Sprintf("%db", memLimitBytes)}
	}

	if len(out) == 0 {
		return "", nil
	}

	doc := map[string]any{"services": out}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("failed to marshal GPU memory override: %w", err)
	}

	header := "# Citadel per-service RAM ceiling override (auto-generated, citadel#831).\n" +
		"# Applied to GPU/media services on top of the base compose via\n" +
		"#   <runtime> compose -f <name>.yml -f <name>.ram.yml up\n" +
		"# Sets mem_limit ONLY -- no capability changes, no read-only rootfs (the\n" +
		"# #348 GPU hardening exemption in sandbox.go stays in force). A runaway\n" +
		"# process is killed by its OWN cgroup limit rather than the host's global\n" +
		"# OOM killer picking an arbitrary victim across containers. Regenerated on\n" +
		"# every SERVICE_START while resource isolation is enabled (CITADEL_RESOURCE_\n" +
		"# ISOLATION); see docs/design-resource-isolation.md.\n"
	return header + string(data), nil
}
