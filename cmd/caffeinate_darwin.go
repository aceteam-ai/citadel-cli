//go:build darwin

package cmd

import (
	"context"
	"fmt"
	"os/exec"
)

// startCaffeinate starts the macOS caffeinate utility to prevent the system from
// sleeping while the worker is active. Uses -dimsu to prevent display sleep,
// idle sleep, disk sleep, and system sleep.
//
// Returns a cancel function that stops the caffeinate process. If caffeinate
// is not available, logs a warning and returns a no-op cancel function.
func startCaffeinate(ctx context.Context) func() {
	path, err := exec.LookPath("caffeinate")
	if err != nil {
		fmt.Println("   - Warning: caffeinate not found, system may sleep during work")
		return func() {}
	}

	cmd := exec.CommandContext(ctx, path, "-dimsu")
	if err := cmd.Start(); err != nil {
		fmt.Printf("   - Warning: failed to start caffeinate: %v\n", err)
		return func() {}
	}

	fmt.Println("   - Sleep prevention: caffeinate active")
	return func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
}
