package platform

import (
	"os/exec"
	"runtime"
	"strings"
)

// gpu_compute_cap.go: NVIDIA compute-capability detection for the TEI GPU-aware
// image-tag resolution (aceteam-ai/citadel-cli#1269). The pure parse
// (parseGPUComputeCaps) is unit-tested off a captured nvidia-smi fixture; the
// exec wrapper (GetGPUComputeCaps) is intentionally thin, and the cmd/jobs
// callers inject it via their OWN package-level seam so no compose-up wiring
// test shells out to a real GPU.

// GetGPUComputeCaps returns the CUDA compute capabilities of the node's NVIDIA
// GPUs, one entry per GPU in nvidia-smi order (e.g. ["8.6"] for an RTX 3090), or
// nil when there is no usable NVIDIA GPU (no driver, query unsupported, or
// macOS/Metal, which has no CUDA compute capability).
func GetGPUComputeCaps() []string {
	if runtime.GOOS == "darwin" {
		return nil
	}
	out, err := nvidiaSmiComputeCapOutput()
	if err != nil {
		return nil
	}
	return parseGPUComputeCaps(out)
}

// nvidiaSmiComputeCapOutput runs
// `nvidia-smi --query-gpu=compute_cap --format=csv,noheader` and returns its
// stdout. On Windows it prefers the standard install path the existing
// WindowsGPUDetector uses, falling back to PATH; elsewhere it uses PATH.
func nvidiaSmiComputeCapOutput() (string, error) {
	args := []string{"--query-gpu=compute_cap", "--format=csv,noheader"}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = (&WindowsGPUDetector{}).nvidiaSmiCommand(args...)
	} else {
		cmd = exec.Command("nvidia-smi", args...)
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// parseGPUComputeCaps parses the CSV output of
// `nvidia-smi --query-gpu=compute_cap --format=csv,noheader` (one compute
// capability per line, e.g. "8.6") into a slice in nvidia-smi order, skipping
// blank lines and trimming surrounding whitespace. Pure and unit-tested against
// fixtures, independent of a real nvidia-smi binary.
func parseGPUComputeCaps(output string) []string {
	var caps []string
	for _, line := range strings.Split(output, "\n") {
		if v := strings.TrimSpace(line); v != "" {
			caps = append(caps, v)
		}
	}
	return caps
}
