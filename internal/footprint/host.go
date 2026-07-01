package footprint

import (
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
)

// hostCPUPercent returns the host-wide CPU utilisation percentage. The 100ms
// sample is cheap and matches the status collector's cadence. Returns (0, false)
// on read error.
func hostCPUPercent() (float64, bool) {
	pcts, err := cpu.Percent(100*time.Millisecond, false)
	if err != nil || len(pcts) == 0 {
		return 0, false
	}
	return pcts[0], true
}

// hostRSSMB returns the host used-memory in MB. Returns (0, false) on read
// error.
func hostRSSMB() (float64, bool) {
	v, err := mem.VirtualMemory()
	if err != nil {
		return 0, false
	}
	return float64(v.Used) / (1024 * 1024), true
}
