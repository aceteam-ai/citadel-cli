//go:build !linux

package status

import (
	"runtime"

	"github.com/shirou/gopsutil/v3/load"
)

func nativeCapacityProbes() capacityProbes {
	return capacityProbes{
		ncpu: runtime.NumCPU,
		load: func() ([3]float64, error) {
			values, err := load.Avg()
			if err != nil || values == nil {
				return [3]float64{}, errCapacityUnavailable
			}
			return [3]float64{values.Load1, values.Load5, values.Load15}, nil
		},
		// PSI is Linux-only. Unsupported load sources (including Windows)
		// likewise stay absent, rather than copying CPU utilization or zero.
	}
}
