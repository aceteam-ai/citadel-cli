package network

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain also protects pre-existing tests that use the real default getters
// and state helpers. Pure resolver tests retain their explicit inputs, and tests
// of the machine-pointer helpers may install narrower owned directories.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "citadel-network-fixtures-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: network fixture root: %v\n", err)
		os.Exit(1)
	}
	node, machine := filepath.Join(root, "node"), filepath.Join(root, "machine")
	for _, dir := range []string{node, machine} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			fmt.Fprintf(os.Stderr, "TestMain: network fixture directory: %v\n", err)
			_ = os.RemoveAll(root)
			os.Exit(1)
		}
	}
	restore, err := OverrideStateDirectoriesForTesting(node, machine)
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: network fixture install: %v\n", err)
		_ = os.RemoveAll(root)
		os.Exit(1)
	}
	cleanup := func(code int) {
		if err := restore(); err != nil {
			fmt.Fprintf(os.Stderr, "TestMain: network fixture restore: %v\n", err)
			os.Exit(1) // Preserve directories while any scope still owns them.
		}
		if err := os.RemoveAll(root); err != nil {
			fmt.Fprintf(os.Stderr, "TestMain: network fixture cleanup: %v\n", err)
			code = 1
		}
		os.Exit(code)
	}
	// Refuse before allowing any test to write through a regressed selector.
	if machineStatePointerPath() != filepath.Join(machine, machineStatePointerFile) {
		fmt.Fprintln(os.Stderr, "TestMain: machine pointer escaped network fixture")
		cleanup(1)
	}
	if GetStateDir() != filepath.Join(node, "network") || GetNodeConfigDir() != node {
		fmt.Fprintln(os.Stderr, "TestMain: state directory escaped network fixture")
		cleanup(1)
	}
	cleanup(m.Run())
}
