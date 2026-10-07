package cmd

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestRootPersistentPreRunKeepsMemoryHookStdoutClean(t *testing.T) {
	origDebug := debugToStderr
	origCheck := checkForUpdateOnStartupFn
	t.Cleanup(func() {
		debugToStderr = origDebug
		checkForUpdateOnStartupFn = origCheck
	})

	for _, hook := range []*cobra.Command{memoryRecallCmd, memoryCaptureCmd} {
		t.Run(hook.Name(), func(t *testing.T) {
			debugToStderr = false
			checked := false
			checkForUpdateOnStartupFn = func(bool) { checked = true }
			rootCmd.PersistentPreRun(hook, nil)
			if checked {
				t.Fatal("memory hook performed an update check that could contaminate stdout")
			}
			if !debugToStderr {
				t.Fatal("memory hook debug output was not redirected to stderr")
			}
		})
	}
}
