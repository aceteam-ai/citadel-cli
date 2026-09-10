package cmd

import (
	"errors"
	"testing"
)

// TestEnsureMachineStatePointerForRootWorker pins the #1017 wiring: the
// network-only init path records the machine state pointer ONLY when root, and
// passes the resolved state dir straight through to
// network.EnsureMachineStatePointer. The defer registration in the init Run
// closure is inspection-verified (a defer at the top of the network-only branch);
// this test covers the gate + call it fires.
func TestEnsureMachineStatePointerForRootWorker(t *testing.T) {
	origRoot := initIsRootFn
	origState := initGetStateDirFn
	origEnsure := initEnsureMachineStatePointerFn
	t.Cleanup(func() {
		initIsRootFn = origRoot
		initGetStateDirFn = origState
		initEnsureMachineStatePointerFn = origEnsure
	})

	t.Run("non-root skips the write entirely", func(t *testing.T) {
		initIsRootFn = func() bool { return false }
		initGetStateDirFn = func() string { return "/should/not/matter/network" }
		called := false
		initEnsureMachineStatePointerFn = func(string) error { called = true; return nil }

		ensureMachineStatePointerForRootWorker()
		if called {
			t.Fatal("non-root init must not attempt to write the machine state pointer")
		}
	})

	t.Run("root writes the pointer with the resolved state dir", func(t *testing.T) {
		initIsRootFn = func() bool { return true }
		initGetStateDirFn = func() string { return "/home/human/citadel-node/network" }
		var gotStateDir string
		initEnsureMachineStatePointerFn = func(sd string) error { gotStateDir = sd; return nil }

		ensureMachineStatePointerForRootWorker()
		if gotStateDir != "/home/human/citadel-node/network" {
			t.Fatalf("EnsureMachineStatePointer got %q, want the resolved state dir", gotStateDir)
		}
	})

	t.Run("a write error is non-fatal (best effort)", func(t *testing.T) {
		initIsRootFn = func() bool { return true }
		initGetStateDirFn = func() string { return "/x/network" }
		initEnsureMachineStatePointerFn = func(string) error { return errors.New("permission denied") }
		// Must not panic; the helper swallows the error after logging.
		ensureMachineStatePointerForRootWorker()
	})
}
