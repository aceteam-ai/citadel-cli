// cmd/connect_tmux_test.go
//
// Pins the citadel #759 CLI-side contract: 'citadel ssh'/'citadel connect'
// request a bare shell by DEFAULT, and --tmux is what opts a single
// connection into the node's persistent, reconnect-resilient session. Both
// are exercised through connectToNode -> terminalAttemptFn, the same seam
// cmd/connect_dispatch_test.go already uses, so these tests need no live
// mesh or terminal server.
package cmd

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/terminal"
)

func TestConnectToNode_DefaultSendsBareSessionOverride(t *testing.T) {
	resetDispatchState(t)

	var gotSession string
	terminalAttemptFn = func(target, passcode, session string) error {
		gotSession = session
		return nil
	}

	if err := connectToNode(cmdWithChangedFlags(nil), "gpu-node-1"); err != nil {
		t.Fatalf("connectToNode() error = %v, want nil", err)
	}
	if gotSession != bareSessionOverride {
		t.Errorf("session = %q, want %q (bare by default)", gotSession, bareSessionOverride)
	}
}

func TestConnectToNode_NoTmuxSendsBareSessionOverride(t *testing.T) {
	resetDispatchState(t)
	wantNoTmuxSession = true

	var gotSession string
	terminalAttemptFn = func(target, passcode, session string) error {
		gotSession = session
		return nil
	}

	if err := connectToNode(cmdWithChangedFlags(nil), "gpu-node-1"); err != nil {
		t.Fatalf("connectToNode() error = %v, want nil", err)
	}
	if gotSession != bareSessionOverride {
		t.Errorf("session = %q, want %q (--no-tmux is explicit bare)", gotSession, bareSessionOverride)
	}
}

func TestConnectToNode_TmuxSendsNamedSessionOverride(t *testing.T) {
	resetDispatchState(t)
	wantTmuxSession = true

	var gotSession string
	terminalAttemptFn = func(target, passcode, session string) error {
		gotSession = session
		return nil
	}

	if err := connectToNode(cmdWithChangedFlags(nil), "gpu-node-1"); err != nil {
		t.Fatalf("connectToNode() error = %v, want nil", err)
	}
	if gotSession != terminal.DefaultSessionName {
		t.Errorf("session = %q, want %q (--tmux requests the node's persistent session name)", gotSession, terminal.DefaultSessionName)
	}
	if gotSession == bareSessionOverride {
		t.Fatalf("session = %q, must not equal the bare override %q when --tmux is set", gotSession, bareSessionOverride)
	}
}

func TestConnectToNode_TmuxAndNoTmuxConflict(t *testing.T) {
	resetDispatchState(t)
	wantTmuxSession, wantNoTmuxSession = true, true

	terminalAttemptFn = func(string, string, string) error {
		t.Fatal("terminalAttemptFn must not be called when --tmux and --no-tmux conflict")
		return nil
	}
	legacySSHFn = func(string) error {
		t.Fatal("legacySSHFn must not be called when --tmux and --no-tmux conflict")
		return nil
	}

	err := connectToNode(cmdWithChangedFlags(nil), "gpu-node-1")
	if err == nil {
		t.Fatal("connectToNode() error = nil, want a mutually-exclusive-flags error")
	}
}

// TestSSHConnectTmuxFlagsRegistered pins that both 'citadel ssh' and
// 'citadel connect' register --tmux/--no-tmux (the alias contract every
// other shared flag in this package already gets, see
// cmd/ssh_connect_alias_test.go's TestSSHConnectSharedFlagsPresent).
func TestSSHConnectTmuxFlagsRegistered(t *testing.T) {
	for _, name := range []string{"tmux", "no-tmux"} {
		if sshCmd.Flags().Lookup(name) == nil {
			t.Errorf("sshCmd missing --%s", name)
		}
		if connectCmd.Flags().Lookup(name) == nil {
			t.Errorf("connectCmd missing --%s", name)
		}
	}
}
