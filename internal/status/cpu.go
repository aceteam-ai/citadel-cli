package status

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// CPUPercent returns CPU utilization percentages, mirroring the semantics of
// gopsutil's cpu.Percent: with percpu=false it returns a single aggregate
// value (0-100), and it samples over the given interval.
//
// It exists because gopsutil's darwin cpu.Percent requires cgo (see
// cpu_darwin.go). Citadel's release and CI builds compile with CGO_ENABLED=0,
// so on those darwin binaries gopsutil returns ErrNotImplementedError and
// `citadel status` rendered "Error getting CPU info: not implemented yet"
// (issue #1053). The platform-specific cpuPercent (cpu_default.go /
// cpu_darwin.go) supplies a native fallback on darwin while leaving every
// other platform's behavior identical to a direct gopsutil call.
func CPUPercent(interval time.Duration, percpu bool) ([]float64, error) {
	return cpuPercent(interval, percpu)
}

// topCPUUsageRe matches a `top` "CPU usage:" summary line, e.g.
//
//	CPU usage: 10.52% user, 8.77% sys, 80.70% idle
//
// The percentages are captured so the aggregate busy fraction can be derived
// as 100 - idle (identical to gopsutil's busy/total accounting).
var topCPUUsageRe = regexp.MustCompile(
	`CPU usage:\s*([0-9.]+)%\s*user,\s*([0-9.]+)%\s*sys,\s*([0-9.]+)%\s*idle`)

// parseTopCPUUsage extracts the aggregate CPU utilization (0-100) from the
// output of `top -l 2 -n 0` on darwin. `top -l 2` emits two "CPU usage:"
// lines: the first is cumulative since boot, the second is the delta over the
// sampling interval. The last match is therefore the accurate instantaneous
// reading, so this returns the utilization of the LAST "CPU usage:" line.
//
// It is a pure function (no exec) so it can be unit-tested on Linux CI where
// the darwin collection path itself cannot run.
func parseTopCPUUsage(output string) (float64, error) {
	matches := topCPUUsageRe.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return 0, fmt.Errorf("no 'CPU usage:' line found in top output")
	}
	last := matches[len(matches)-1]
	idle, err := strconv.ParseFloat(last[3], 64)
	if err != nil {
		return 0, fmt.Errorf("parsing idle percentage %q: %w", last[3], err)
	}
	busy := 100 - idle
	if busy < 0 {
		busy = 0
	}
	if busy > 100 {
		busy = 100
	}
	return busy, nil
}
