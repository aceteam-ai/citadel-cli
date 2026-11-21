//go:build !windows

package cmd

import (
	"os"
	"syscall"
)

// hpcDrainSignals are the early drain signals (Slurm `--signal=B:USR1@N`).
var hpcDrainSignals = []os.Signal{syscall.SIGUSR1}
