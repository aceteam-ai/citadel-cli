package platform

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ComposeGPUProbeTimeout bounds both commands together, including a hung driver.
const ComposeGPUProbeTimeout = 3 * time.Second

// NvidiaGPUCountForCompose returns a positive count, zero for proven absence,
// or -1 for uncertainty. Unlike GetGPUCountSimple, probe failure is not absence.
func NvidiaGPUCountForCompose() int {
	return composeGPUCount(context.Background(), func(ctx context.Context, name string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.WaitDelay = 100 * time.Millisecond
		return cmd.Output()
	}, os.Stat, os.ReadFile)
}

func composeGPUCount(parent context.Context, run func(context.Context, string, ...string) ([]byte, error), stat func(string) (os.FileInfo, error), readFile func(string) ([]byte, error)) int {
	ctx, cancel := context.WithTimeout(parent, ComposeGPUProbeTimeout)
	defer cancel()
	output, queryErr := run(ctx, "nvidia-smi", "--query-gpu=name", "--format=csv,noheader")
	if ctx.Err() != nil {
		return -1
	}
	if queryErr == nil && strings.TrimSpace(string(output)) != "" {
		return len(strings.Split(strings.TrimSpace(string(output)), "\n"))
	}
	// Hardware evidence wins even when the driver/query is broken or unavailable.
	if _, err := stat("/dev/nvidiactl"); err == nil || !os.IsNotExist(err) {
		return -1
	}
	// Tegra GPUs are integrated, so PCI absence is not GPU absence. Driver
	// tools and /dev/nvidiactl may both be absent on these hosts. Evidence of
	// Tegra, or an unreadable hardware marker, preserves the existing flavor.
	for _, path := range []string{"/etc/nv_tegra_release", "/dev/nvhost-ctrl-gpu"} {
		if _, err := stat(path); err == nil || !os.IsNotExist(err) {
			return -1
		}
	}
	compatible, err := readFile("/proc/device-tree/compatible")
	if (err != nil && !os.IsNotExist(err)) || strings.Contains(strings.ToLower(string(compatible)), "nvidia,tegra") || ctx.Err() != nil {
		return -1
	}
	hardware, err := run(ctx, "lspci", "-nn")
	if err != nil || ctx.Err() != nil {
		return -1
	}
	for _, line := range strings.Split(strings.ToLower(string(hardware)), "\n") {
		if strings.Contains(line, "nvidia") && (strings.Contains(line, "vga compatible controller") || strings.Contains(line, "3d controller") || strings.Contains(line, "display controller")) {
			return -1
		}
	}
	// A nonempty failed query is ambiguous; never replace an existing flavor.
	if queryErr != nil && strings.TrimSpace(string(output)) != "" {
		return -1
	}
	return 0
}
