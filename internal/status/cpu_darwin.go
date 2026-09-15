//go:build darwin

package status

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
)

// cpuPercent returns CPU utilization on darwin.
//
// gopsutil's cpu.Percent works on darwin only when built with cgo (it reads
// mach host_statistics via C). Citadel's release/CI binaries are built with
// CGO_ENABLED=0, so gopsutil's non-cgo darwin path returns
// ErrNotImplementedError. This first tries gopsutil (which succeeds on a
// cgo-enabled darwin build) and only falls back to the native `top`-based
// sampler when gopsutil yields no usable reading, so both build modes report a
// real number.
func cpuPercent(interval time.Duration, percpu bool) ([]float64, error) {
	if p, err := cpu.Percent(interval, percpu); err == nil && len(p) > 0 {
		return p, nil
	}
	return darwinCPUPercentFromTop(interval, percpu)
}

// darwinCPUPercentFromTop samples aggregate CPU usage via `top -l 2 -n 0`.
//
// `top -l 2` takes two samples ~1s apart; the second "CPU usage:" line is the
// delta over that interval (the first is cumulative since boot). `-n 0`
// suppresses the per-process rows so the output is small and fast to parse.
//
// The sub-second intervals the callers request (100ms/200ms/500ms) cannot be
// honored by top, which samples in whole seconds; the ~1s window is used
// instead. Per-CPU output (percpu=true) is not supported by this fallback (no
// caller requests it); it returns the aggregate as a single-element slice.
func darwinCPUPercentFromTop(interval time.Duration, percpu bool) ([]float64, error) {
	// Generous bound: top -l 2 takes ~1s; never let a wedged subprocess hang
	// a status collection or heartbeat.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "top", "-l", "2", "-n", "0").Output()
	if err != nil {
		return nil, fmt.Errorf("running top for CPU usage: %w", err)
	}
	busy, err := parseTopCPUUsage(string(out))
	if err != nil {
		return nil, err
	}
	return []float64{busy}, nil
}
