// cmd/root_mcp_debug_order_test.go
package cmd

import (
	"testing"

	"github.com/spf13/cobra"
)

// TestRootPersistentPreRunRoutesMCPDebugBeforeSwapRecovery is the regression
// test for citadel#934.
//
// PersistentPreRun's citadel#926 RecoverInterruptedSwap() call can itself
// emit Debug()/Log() output. Those helpers decide stdout-vs-stderr by reading
// the package-level debugToStderr var at the moment they're called -- so
// whichever of "set debugToStderr" and "call RecoverInterruptedSwap" runs
// first determines where that output lands. For `citadel mcp`, stdout IS the
// JSON-RPC transport, so debugToStderr must already be true by the time
// RecoverInterruptedSwap runs, not just by the time PersistentPreRun returns.
//
// This test can't observe the ordering of the two calls directly (both are
// side-effect-free on a non-Windows test host -- see
// TestRecoverInterruptedSwap_ExposedWrapperIsWindowsScoped in
// internal/update), but it pins the externally-visible half of the contract:
// after PersistentPreRun runs for a command named "mcp", debugToStderr is
// true. Combined with a source read confirming the debugToStderr assignment
// precedes the RecoverInterruptedSwap call in root.go, this closes the loop.
func TestRootPersistentPreRunRoutesMCPDebugBeforeSwapRecovery(t *testing.T) {
	if rootCmd.PersistentPreRun == nil {
		t.Fatal("rootCmd has no PersistentPreRun to test")
	}

	orig := debugToStderr
	t.Cleanup(func() { debugToStderr = orig })

	debugToStderr = false
	mcpLike := &cobra.Command{Use: "mcp"}
	rootCmd.PersistentPreRun(mcpLike, nil)

	if !debugToStderr {
		t.Error("PersistentPreRun must set debugToStderr for the mcp command " +
			"(citadel#934) -- otherwise citadel#926's RecoverInterruptedSwap() " +
			"debug/log calls can corrupt the MCP JSON-RPC stdout transport")
	}
}

// TestRootPersistentPreRunLeavesDebugStreamAloneForOtherCommands guards
// against the fix over-applying: only the mcp command should force debug
// output to stderr.
func TestRootPersistentPreRunLeavesDebugStreamAloneForOtherCommands(t *testing.T) {
	if rootCmd.PersistentPreRun == nil {
		t.Fatal("rootCmd has no PersistentPreRun to test")
	}

	orig := debugToStderr
	t.Cleanup(func() { debugToStderr = orig })

	debugToStderr = false
	other := &cobra.Command{Use: "status"}
	rootCmd.PersistentPreRun(other, nil)

	if debugToStderr {
		t.Error("PersistentPreRun must not force debugToStderr for non-mcp commands")
	}
}
