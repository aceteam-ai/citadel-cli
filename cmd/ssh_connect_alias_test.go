// cmd/ssh_connect_alias_test.go
//
// Pins the #754 "mutual alias" contract between `citadel ssh` and
// `citadel connect`: same shared flags, each command's help names the
// other, and (the part that actually matters) identical routing behavior
// for the same bare target regardless of which command name was invoked.
package cmd

import (
	"context"
	"strings"
	"testing"
)

// TestSSHConnectSharedFlagsPresent asserts sshCmd and connectCmd register
// the same #754 override flags (bound to the same package vars, so setting
// one via either command name has the same effect). -u/-p/-v are
// deliberately asymmetric (sshCmd only); see TestConnectCmdHasNoRawSSHDFlags.
func TestSSHConnectSharedFlagsPresent(t *testing.T) {
	shared := []string{"raw", "via-sshd", "mesh", "terminal", "passcode", "tmux", "no-tmux"}
	for _, name := range shared {
		if sshCmd.Flags().Lookup(name) == nil {
			t.Errorf("sshCmd missing shared flag --%s", name)
		}
		if connectCmd.Flags().Lookup(name) == nil {
			t.Errorf("connectCmd missing shared flag --%s", name)
		}
	}
}

// TestConnectCmdHasNoRawSSHDFlags documents the deliberate scope decision
// (see PR description): -u/--user, -p/--port, -v/--verbose for the raw-sshd
// fallback stay ssh-only. They control a real OpenSSH invocation's identity
// and port, which only 'citadel ssh' builds; 'citadel connect' does not gain
// them just to claim full flag parity.
func TestConnectCmdHasNoRawSSHDFlags(t *testing.T) {
	for _, name := range []string{"user", "port", "verbose"} {
		if connectCmd.Flags().Lookup(name) != nil {
			t.Errorf("connectCmd unexpectedly has --%s (deliberately ssh-only)", name)
		}
	}
	for _, name := range []string{"user", "port", "verbose"} {
		if sshCmd.Flags().Lookup(name) == nil {
			t.Errorf("sshCmd missing --%s", name)
		}
	}
}

// TestSSHConnectHelpNameEachOther asserts each command's Long help text
// documents the alias relationship, so a user reading either --help learns
// about the other command.
func TestSSHConnectHelpNameEachOther(t *testing.T) {
	if !strings.Contains(sshCmd.Long, "citadel connect") {
		t.Error("sshCmd.Long does not mention 'citadel connect'")
	}
	if !strings.Contains(connectCmd.Long, "citadel ssh") {
		t.Error("connectCmd.Long does not mention 'citadel ssh'")
	}
}

// TestSSHAndConnectRouteBareTargetIdentically drives both commands' real Run
// functions with the same bare target and asserts they exercise the exact
// same shared routing (connectToNode -> terminalAttemptFn) with the same
// target: the behavioral proof that they are aliases, not just two
// commands that happen to look similar. Network connectivity and peer
// resolution are stubbed out (ensureNetworkConnectedFn, and an IP target so
// resolvePeer's fast path never touches the network) so this runs without a
// live mesh.
func TestSSHAndConnectRouteBareTargetIdentically(t *testing.T) {
	resetDispatchState(t)

	const target = "100.64.0.9" // valid IP -> resolvePeer's isValidIP fast path, no network call

	var calls []string
	terminalAttemptFn = func(gotTarget, passcode, session string) error {
		calls = append(calls, gotTarget)
		return nil
	}
	legacySSHFn = func(string) error {
		t.Fatal("legacySSHFn must not be called on ts-net success")
		return nil
	}

	sshCmd.Run(sshCmd, []string{target})
	connectCmd.Run(connectCmd, []string{target})

	if len(calls) != 2 {
		t.Fatalf("terminalAttemptFn called %d times, want 2 (once per command)", len(calls))
	}
	if calls[0] != target || calls[1] != target {
		t.Fatalf("calls = %v, want both %q", calls, target)
	}
}

// ensureNetworkConnectedFn is stubbed to nil-out; sanity-check that helper
// exists and behaves (guards against a future refactor silently dropping the
// injection point that makes the test above possible without a live mesh).
func TestEnsureNetworkConnectedFnIsInjectable(t *testing.T) {
	resetDispatchState(t)
	called := false
	ensureNetworkConnectedFn = func(context.Context) error {
		called = true
		return nil
	}
	if err := ensureNetworkConnectedFn(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("stub was not invoked")
	}
}
