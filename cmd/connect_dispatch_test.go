// cmd/connect_dispatch_test.go
package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// resetDispatchState saves and restores every package var connectToNode's
// routing depends on, plus the injectable function vars. The cmd package's
// tests share one binary, so an unrestored override in one test would leak
// into every test that runs after it.
func resetDispatchState(t *testing.T) {
	t.Helper()
	origViaRaw, origViaMesh, origPasscode := viaRaw, viaMesh, shellPasscode
	origSSHUser, origSSHPort := sshUser, sshPort
	origTerminalAttempt := terminalAttemptFn
	origLegacySSH := legacySSHFn
	origPromptPasscode := promptPasscodeFn
	origEnsureNetwork := ensureNetworkConnectedFn

	t.Cleanup(func() {
		viaRaw, viaMesh, shellPasscode = origViaRaw, origViaMesh, origPasscode
		sshUser, sshPort = origSSHUser, origSSHPort
		terminalAttemptFn = origTerminalAttempt
		legacySSHFn = origLegacySSH
		promptPasscodeFn = origPromptPasscode
		ensureNetworkConnectedFn = origEnsureNetwork
	})

	viaRaw, viaMesh, shellPasscode = false, false, ""
	sshUser, sshPort = "", ""
	ensureNetworkConnectedFn = func(context.Context) error { return nil }
}

// cmdWithChangedFlags returns a throwaway *cobra.Command carrying the same
// "user"/"port" flags sshCmd registers, with the named ones marked as
// explicitly set (pflag's Set() marks Changed=true), so connectToNode's
// cmd.Flags().Changed("user"/"port") check can be driven without touching
// the real sshCmd/connectCmd singletons (which every other test in this
// package also uses).
func cmdWithChangedFlags(changed []string) *cobra.Command {
	c := &cobra.Command{Use: "test"}
	var u, p string
	c.Flags().StringVarP(&u, "user", "u", "", "")
	c.Flags().StringVarP(&p, "port", "p", "", "")
	for _, name := range changed {
		_ = c.Flags().Set(name, "x")
	}
	return c
}

func TestConnectToNode_TsNetSuccess(t *testing.T) {
	resetDispatchState(t)

	var gotTarget, gotPasscode string
	var calls int
	terminalAttemptFn = func(target, passcode string) error {
		calls++
		gotTarget, gotPasscode = target, passcode
		return nil
	}
	legacySSHFn = func(string) error {
		t.Fatal("legacySSHFn must not be called on ts-net success")
		return nil
	}

	if err := connectToNode(cmdWithChangedFlags(nil), "gpu-node-1"); err != nil {
		t.Fatalf("connectToNode() error = %v, want nil", err)
	}
	if calls != 1 {
		t.Fatalf("terminalAttemptFn called %d times, want 1", calls)
	}
	if gotTarget != "gpu-node-1" {
		t.Errorf("target = %q, want %q", gotTarget, "gpu-node-1")
	}
	if gotPasscode != "" {
		t.Errorf("passcode = %q, want empty (none configured)", gotPasscode)
	}
}

func TestConnectToNode_UnreachableFallsBackToRawSSHD(t *testing.T) {
	resetDispatchState(t)

	terminalAttemptFn = func(target, passcode string) error {
		return &terminalDialError{kind: terminalDialErrUnreachable, err: errors.New("connection refused")}
	}
	var legacyCalls int
	var legacyTarget string
	legacySSHFn = func(target string) error {
		legacyCalls++
		legacyTarget = target
		return nil
	}

	if err := connectToNode(cmdWithChangedFlags(nil), "gpu-node-1"); err != nil {
		t.Fatalf("connectToNode() error = %v, want nil", err)
	}
	if legacyCalls != 1 {
		t.Fatalf("legacySSHFn called %d times, want 1 (fallback on unreachable)", legacyCalls)
	}
	if legacyTarget != "gpu-node-1" {
		t.Errorf("legacySSHFn target = %q, want %q", legacyTarget, "gpu-node-1")
	}
}

// TestRawFallbackNoticeIsActionable pins the substance of the message
// printed right before falling back to raw sshd. This has to carry the real
// guidance up front: runLegacySSH calls os.Exit directly on an ssh(1)
// failure, so if the raw-sshd fallback ALSO fails (the exact #754 report:
// an embedded-tsnet node with no host sshd exposed on the mesh), nothing
// printed afterward would ever be seen, and the operator is left staring at
// OpenSSH's own opaque "Connection closed by UNKNOWN port 65535" with no
// citadel-authored guidance at all.
func TestRawFallbackNoticeIsActionable(t *testing.T) {
	for _, want := range []string{"raw sshd", "citadel work", "--mesh"} {
		if !strings.Contains(rawFallbackNotice, want) {
			t.Errorf("rawFallbackNotice = %q, want it to mention %q", rawFallbackNotice, want)
		}
	}
}

func TestConnectToNode_MeshOnly_NoFallbackOnUnreachable(t *testing.T) {
	resetDispatchState(t)
	viaMesh = true

	wantErr := &terminalDialError{kind: terminalDialErrUnreachable, err: errors.New("connection refused")}
	terminalAttemptFn = func(target, passcode string) error { return wantErr }
	legacySSHFn = func(string) error {
		t.Fatal("legacySSHFn must not be called with --mesh forcing ts-net only")
		return nil
	}

	err := connectToNode(cmdWithChangedFlags(nil), "gpu-node-1")
	var tdErr *terminalDialError
	if !errors.As(err, &tdErr) || tdErr.kind != terminalDialErrUnreachable {
		t.Fatalf("connectToNode() error = %v, want the unreachable dial error surfaced directly (no fallback under --mesh)", err)
	}
}

func TestConnectToNode_PasscodePromptAndRetry(t *testing.T) {
	resetDispatchState(t)

	var attempts []string // passcodes seen by terminalAttemptFn, in order
	terminalAttemptFn = func(target, passcode string) error {
		attempts = append(attempts, passcode)
		if passcode == "correct-horse" {
			return nil
		}
		return &terminalDialError{kind: terminalDialErrPasscode, err: errors.New("node passcode required")}
	}
	var promptCalls int
	promptPasscodeFn = func(prompt string) (string, error) {
		promptCalls++
		return "correct-horse", nil
	}
	legacySSHFn = func(string) error {
		t.Fatal("legacySSHFn must not be called for a passcode rejection (#754: not a fallback trigger)")
		return nil
	}

	if err := connectToNode(cmdWithChangedFlags(nil), "gpu-node-1"); err != nil {
		t.Fatalf("connectToNode() error = %v, want nil after a successful retry", err)
	}
	if promptCalls != 1 {
		t.Fatalf("promptPasscodeFn called %d times, want 1", promptCalls)
	}
	if len(attempts) != 2 {
		t.Fatalf("terminalAttemptFn called %d times, want 2 (initial + retry)", len(attempts))
	}
	if attempts[0] != "" {
		t.Errorf("first attempt passcode = %q, want empty", attempts[0])
	}
	if attempts[1] != "correct-horse" {
		t.Errorf("retry passcode = %q, want %q", attempts[1], "correct-horse")
	}
}

func TestConnectToNode_PasscodeRejectedExhausted_NeverFallsBack(t *testing.T) {
	resetDispatchState(t)

	var attempts int
	terminalAttemptFn = func(target, passcode string) error {
		attempts++
		return &terminalDialError{kind: terminalDialErrPasscode, err: errors.New("node passcode required")}
	}
	var promptCalls int
	promptPasscodeFn = func(prompt string) (string, error) {
		promptCalls++
		return "still-wrong", nil
	}
	legacySSHFn = func(string) error {
		t.Fatal("legacySSHFn must not be called after repeated passcode rejections (#754: not a fallback trigger)")
		return nil
	}

	err := connectToNode(cmdWithChangedFlags(nil), "gpu-node-1")
	if err == nil {
		t.Fatal("connectToNode() error = nil, want a passcode-rejected error")
	}
	if attempts != maxPasscodeAttempts {
		t.Errorf("terminalAttemptFn called %d times, want %d (maxPasscodeAttempts)", attempts, maxPasscodeAttempts)
	}
	if promptCalls != maxPasscodeAttempts-1 {
		t.Errorf("promptPasscodeFn called %d times, want %d", promptCalls, maxPasscodeAttempts-1)
	}
}

func TestConnectToNode_PasscodeFromEnv(t *testing.T) {
	resetDispatchState(t)
	t.Setenv("CITADEL_TERMINAL_PASSCODE", "env-secret")

	var gotPasscode string
	terminalAttemptFn = func(target, passcode string) error {
		gotPasscode = passcode
		return nil
	}
	legacySSHFn = func(string) error { t.Fatal("unexpected fallback"); return nil }

	if err := connectToNode(cmdWithChangedFlags(nil), "gpu-node-1"); err != nil {
		t.Fatalf("connectToNode() error = %v", err)
	}
	if gotPasscode != "env-secret" {
		t.Errorf("passcode = %q, want %q (from CITADEL_TERMINAL_PASSCODE)", gotPasscode, "env-secret")
	}
}

func TestConnectToNode_RawSkipsTsNet(t *testing.T) {
	resetDispatchState(t)
	viaRaw = true

	terminalAttemptFn = func(string, string) error {
		t.Fatal("terminalAttemptFn must not be called with --raw")
		return nil
	}
	var legacyCalls int
	legacySSHFn = func(string) error { legacyCalls++; return nil }

	if err := connectToNode(cmdWithChangedFlags(nil), "gpu-node-1"); err != nil {
		t.Fatalf("connectToNode() error = %v, want nil", err)
	}
	if legacyCalls != 1 {
		t.Fatalf("legacySSHFn called %d times, want 1", legacyCalls)
	}
}

func TestConnectToNode_RawAndMeshConflict(t *testing.T) {
	resetDispatchState(t)
	viaRaw, viaMesh = true, true

	terminalAttemptFn = func(string, string) error { t.Fatal("unexpected call"); return nil }
	legacySSHFn = func(string) error { t.Fatal("unexpected call"); return nil }

	err := connectToNode(cmdWithChangedFlags(nil), "gpu-node-1")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("connectToNode() error = %v, want a mutually-exclusive-flags error", err)
	}
}

func TestConnectToNode_PortOrUserImpliesRaw(t *testing.T) {
	for _, flag := range []string{"port", "user"} {
		t.Run(flag, func(t *testing.T) {
			resetDispatchState(t)

			terminalAttemptFn = func(string, string) error {
				t.Fatal("terminalAttemptFn must not be called when -p/-u was given")
				return nil
			}
			var legacyCalls int
			legacySSHFn = func(string) error { legacyCalls++; return nil }

			if err := connectToNode(cmdWithChangedFlags([]string{flag}), "gpu-node-1"); err != nil {
				t.Fatalf("connectToNode() error = %v, want nil", err)
			}
			if legacyCalls != 1 {
				t.Fatalf("legacySSHFn called %d times, want 1 (implied by -%s)", legacyCalls, flag)
			}
		})
	}
}

func TestConnectToNode_PortOrUserConflictsWithMesh(t *testing.T) {
	resetDispatchState(t)
	viaMesh = true

	terminalAttemptFn = func(string, string) error { t.Fatal("unexpected call"); return nil }
	legacySSHFn = func(string) error { t.Fatal("unexpected call"); return nil }

	err := connectToNode(cmdWithChangedFlags([]string{"port"}), "gpu-node-1")
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("connectToNode() error = %v, want a conflict error", err)
	}
}

func TestConnectToNode_OtherErrorSurfacedDirectly(t *testing.T) {
	resetDispatchState(t)

	wantErr := fmt.Errorf("terminal handshake with node failed (HTTP 503)")
	terminalAttemptFn = func(string, string) error { return wantErr }
	legacySSHFn = func(string) error {
		t.Fatal("legacySSHFn must not be called for a non-passcode, non-refused error")
		return nil
	}
	promptPasscodeFn = func(string) (string, error) {
		t.Fatal("promptPasscodeFn must not be called for a non-passcode error")
		return "", nil
	}

	err := connectToNode(cmdWithChangedFlags(nil), "gpu-node-1")
	if err != wantErr {
		t.Fatalf("connectToNode() error = %v, want %v (surfaced as-is)", err, wantErr)
	}
}

// TestBuildSSHArgs_NeverCarriesPasscode pins the security invariant behind
// "never put the passcode in argv" (#754 PR): buildSSHArgs has no passcode
// parameter, so it structurally cannot leak one into the ssh(1) argv even if
// a node passcode is configured (subprocess argv is visible to other users
// on the same machine via `ps`).
func TestBuildSSHArgs_NeverCarriesPasscode(t *testing.T) {
	resetDispatchState(t)
	shellPasscode = "super-secret-passcode"

	args := buildSSHArgs("/usr/local/bin/citadel", "100.64.0.5", "22", "ubuntu", true)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, shellPasscode) {
		t.Fatalf("buildSSHArgs() = %q, must never contain the node passcode", joined)
	}
	// Sanity: the function does produce the expected shape, so the
	// non-containment above is a meaningful assertion and not vacuous.
	if !strings.Contains(joined, "ProxyCommand=/usr/local/bin/citadel connect 100.64.0.5:22") {
		t.Fatalf("buildSSHArgs() = %q, missing expected ProxyCommand", joined)
	}
	if !strings.Contains(joined, "ubuntu@100.64.0.5") {
		t.Fatalf("buildSSHArgs() = %q, missing expected user@host target", joined)
	}
}
