// cmd/network_logf_test.go
package cmd

import (
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/spf13/cobra"
)

// TestRootPersistentPreRunWiresNetworkLogf is the regression test for #662.
//
// internal/network's logf defaults to a no-op, so a command that never calls
// SetLogf discards every engine diagnostic -- and the discard is INVISIBLE:
// every call site still compiles, still runs, and produces no signal that
// anything was lost. That is what made this cost hours in #643. A no-op logger
// cannot be distinguished from a working one by observing behaviour, so the
// wiring itself has to be asserted.
func TestRootPersistentPreRunWiresNetworkLogf(t *testing.T) {
	if rootCmd.PersistentPreRun == nil {
		t.Fatal("rootCmd has no PersistentPreRun, so nothing wires the network logger")
	}

	rootCmd.PersistentPreRun(rootCmd, nil)

	if !network.LogfConfigured() {
		t.Error("PersistentPreRun must install a real network diagnostic logger; " +
			"without it every command silently discards engine diagnostics (#662)")
	}
}

// TestNoSubcommandShadowsRootPersistentPreRun guards the trap that would undo
// the fix silently.
//
// Cobra runs only the CLOSEST PersistentPreRun in the chain, not all of them. A
// subcommand that grows its own would stop inheriting the root's wiring and go
// back to discarding diagnostics -- with no compile error and no test failure
// anywhere else. If this fires, the new hook must call
// rootCmd.PersistentPreRun(cmd, args) itself.
func TestNoSubcommandShadowsRootPersistentPreRun(t *testing.T) {
	var shadowed []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if sub.PersistentPreRun != nil || sub.PersistentPreRunE != nil {
				shadowed = append(shadowed, sub.CommandPath())
			}
			walk(sub)
		}
	}
	walk(rootCmd)

	if len(shadowed) > 0 {
		t.Errorf("these commands define their own PersistentPreRun, which shadows the root hook "+
			"and drops the network-logf wiring (#662): %s\n"+
			"Each must call rootCmd.PersistentPreRun(cmd, args) itself.",
			strings.Join(shadowed, ", "))
	}
}
