//go:build linux

package status

import "runtime"

func nativeCapacityProbes() capacityProbes {
	return procCapacityProbes(readCapacityFile, runtime.NumCPU)
}
