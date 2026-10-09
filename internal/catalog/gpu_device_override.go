// internal/catalog/gpu_device_override.go
//
// GPU device-reservation override wiring for catalog modules whose compose ships
// the NVIDIA device reservation in a separate compose.gpu.yml (the citadel-services
// convention, e.g. services/voice-clone) rather than inline in compose.yml. At
// install time the override is copied beside the compose as <name>.gpu.yml; the
// managed start path layers it as a second `-f` on a GPU (docker) node.
// aceteam-ai/citadel-cli#1245.
package catalog

import (
	"fmt"
	"os"
	"path/filepath"
)

// GPUDeviceOverridePath returns a module's GPU device-reservation override path
// (<servicesDir>/<name>.gpu.yml).
//
// DISTINCT from ExistingGPURAMOverride (<name>.ram.yml, the citadel#831 per-job
// RAM cgroup ceiling the boot path deliberately does NOT apply): this one is the
// engine's REQUIRED NVIDIA device reservation and IS applied at the managed start
// sites so a GPU module actually gets its GPU.
func GPUDeviceOverridePath(servicesDir, name string) string {
	return filepath.Join(servicesDir, name+".gpu.yml")
}

// ExistingGPUDeviceOverride returns GPUDeviceOverridePath if that file exists,
// else "". A no-op for every module that ships no compose.gpu.yml.
func ExistingGPUDeviceOverride(servicesDir, name string) string {
	p := GPUDeviceOverridePath(servicesDir, name)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// GPUDeviceOverrideFileArg returns the ["-f", <name>.gpu.yml] compose argument
// when a module's GPU device-reservation override exists AND the runtime is
// docker, else nil.
//
// It is intentionally empty on Podman: the override carries a Docker
// deploy.resources.reservations.devices block that Podman consumes via CDI, and
// the node's compose.MaterializePodmanGPUCompose rewrite is applied per-up-site to
// the BASE compose only — layering the raw override on a Podman `up` would hand
// Podman a reservation it cannot parse (worse, citadel-services' compose.gpu.yml
// uses a `count: ${VOICE_CLONE_GPU_COUNT:-all}` interpolation the node's text
// rewriter cannot resolve either). Threading the Podman CDI rewrite through the
// override is a tracked follow-up (aceteam-ai/citadel-cli#1245); until then a GPU
// module on a Podman node relies on its base compose, exactly as before this
// override existed — no regression to any shipped service (none ship a .gpu.yml).
func GPUDeviceOverrideFileArg(servicesDir, name, engineBin string) []string {
	if engineBin == "podman" {
		return nil
	}
	if override := ExistingGPUDeviceOverride(servicesDir, name); override != "" {
		return []string{"-f", override}
	}
	return nil
}

// copyGPUDeviceOverride copies a module's sibling compose.gpu.yml (the NVIDIA
// device reservation its base compose omits) to <servicesDir>/<name>.gpu.yml so a
// managed start can layer it. composeSrcPath is the module's own compose.yml in
// its source directory; the sibling is resolved from that directory.
//
// It is an additive no-op (returns "") when wantGPU is false — a module that
// declares no GPU requirement must never have a stray override applied — or when
// the module ships no compose.gpu.yml.
func copyGPUDeviceOverride(composeSrcPath, servicesDir, name string, wantGPU bool) (string, error) {
	if !wantGPU || composeSrcPath == "" {
		return "", nil
	}
	gpuSrc := filepath.Join(filepath.Dir(composeSrcPath), "compose.gpu.yml")
	if _, err := os.Stat(gpuSrc); err != nil {
		return "", nil // no override shipped — additive no-op
	}
	dest := GPUDeviceOverridePath(servicesDir, name)
	if err := copyFile(gpuSrc, dest); err != nil {
		return "", fmt.Errorf("failed to copy GPU device override for '%s': %w", name, err)
	}
	return dest, nil
}
