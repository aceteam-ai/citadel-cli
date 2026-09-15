//go:build !darwin

package status

import (
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
)

// cpuPercent delegates directly to gopsutil on every non-darwin platform, so
// the Linux (and Windows) behavior is byte-for-byte identical to the previous
// direct cpu.Percent call sites.
func cpuPercent(interval time.Duration, percpu bool) ([]float64, error) {
	return cpu.Percent(interval, percpu)
}
