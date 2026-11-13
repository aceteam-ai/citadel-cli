//go:build windows

package cmd

import "os"

// hpcDrainSignals is empty: the hpc profile refuses Windows before any worker
// starts; this keeps the package building there.
var hpcDrainSignals []os.Signal
