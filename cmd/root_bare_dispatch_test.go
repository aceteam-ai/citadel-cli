package cmd

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
)

// TestRootBareNonInteractiveSkipsCachedUpdateCheck exercises the real Cobra
// PersistentPreRun path, rather than calling runBareCitadel directly. A cached
// update notice is written by checkForUpdateOnStartup before Run would print
// the one-line status, so it must not be called for this invocation shape.
func TestRootBareNonInteractiveSkipsCachedUpdateCheck(t *testing.T) {
	resetBareDispatchState(t)
	bareStdoutIsTTYFn = func() bool { return false }
	bareStdinIsTTYFn = func() bool { return false }
	bareHasMeshStateFn = func() bool { return false }
	bareHasDeviceCredentialsFn = func() bool { return false }

	origCheck := checkForUpdateOnStartupFn
	t.Cleanup(func() { checkForUpdateOnStartupFn = origCheck })
	called := false
	var out bytes.Buffer
	checkForUpdateOnStartupFn = func(bool) {
		called = true
		out.WriteString("cached update notice\\n")
	}

	cmd := &cobra.Command{Use: "citadel"}
	cmd.SetOut(&out)
	rootCmd.PersistentPreRun(cmd, nil)
	runBareCitadel(cmd, nil)
	if called {
		t.Fatal("bare non-interactive invocation must skip update check so cached notice cannot precede status")
	}
	if got, want := out.String(), "Citadel enrollment: unenrolled\n"; got != want {
		t.Fatalf("full bare non-interactive output = %q, want stable one-line status %q", got, want)
	}
}

func TestRootBarePipedStdinSkipsCachedUpdateCheck(t *testing.T) {
	resetBareDispatchState(t)
	bareStdoutIsTTYFn = func() bool { return true }
	bareStdinIsTTYFn = func() bool { return false }

	origCheck := checkForUpdateOnStartupFn
	t.Cleanup(func() { checkForUpdateOnStartupFn = origCheck })
	called := false
	checkForUpdateOnStartupFn = func(bool) { called = true }

	cmd := &cobra.Command{Use: "citadel"}
	rootCmd.PersistentPreRun(cmd, nil)
	if called {
		t.Fatal("piped stdin must be non-interactive before update output can be emitted")
	}
}
