// internal/terminal/session_override_test.go
//
// Pins the citadel #759 server-side contract: a per-connection "session"
// query override (sent only by the CLI connect/ssh path) forces a bare shell
// ("none") or a named persistent session, overriding the node's own
// CITADEL_TERMINAL_SESSION default; an ABSENT override (the web console, and
// every pre-#759 caller) reproduces the exact prior behavior. Also pins the
// on-demand tmux install: it fires only when an override explicitly asked
// for a persistent session that tmux.Resolve can't currently satisfy.
package terminal

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestResolveSessionOverride(t *testing.T) {
	cases := []struct {
		name                   string
		configDefault, request string
		wantBase               string
		wantOverridden         bool
	}{
		{name: "no override: node default wins unchanged", configDefault: "citadel", request: "", wantBase: "citadel", wantOverridden: false},
		{name: "no override, node default off: stays off", configDefault: "none", request: "", wantBase: "none", wantOverridden: false},
		{name: "override forces bare even when node default is a name", configDefault: "citadel", request: "none", wantBase: "none", wantOverridden: true},
		{name: "override forces a named session even when node default is off", configDefault: "none", request: "citadel", wantBase: "citadel", wantOverridden: true},
		{name: "override with the same name as the default is still an override", configDefault: "citadel", request: "citadel", wantBase: "citadel", wantOverridden: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base, overridden := resolveSessionOverride(c.configDefault, c.request)
			if base != c.wantBase || overridden != c.wantOverridden {
				t.Errorf("resolveSessionOverride(%q, %q) = (%q, %v), want (%q, %v)",
					c.configDefault, c.request, base, overridden, c.wantBase, c.wantOverridden)
			}
		})
	}
}

// TestResolveSessionCommand_NoOverridePreservesDefault locks in that an
// absent override (web console, or any caller predating #759) drives
// exactly the same tmux-or-bare decision the server made before this
// feature existed: tmux backing when the node default names a session and
// tmux resolves, bare otherwise, and never triggers the on-demand
// install (that install is scoped to an EXPLICIT override, see the next
// test), so a node whose own default wants tmux but has none installed
// still just falls back exactly as before.
func TestResolveSessionCommand_NoOverridePreservesDefault(t *testing.T) {
	t.Run("tmux available, node default names a session", func(t *testing.T) {
		bin := makeFakeTmux(t)
		installCalled := false
		cmd, name, wanted := resolveSessionCommand("citadel", "", "alice", "/bin/bash", func() bool {
			installCalled = true
			return true
		})
		if !wanted {
			t.Fatal("wantedSession = false, want true (node default names a session)")
		}
		want := []string{bin, "new-session", "-A", "-s", name, "/bin/bash"}
		if !reflect.DeepEqual(cmd, want) {
			t.Errorf("command = %v, want %v", cmd, want)
		}
		if installCalled {
			t.Error("on-demand install must not run when tmux already resolves")
		}
	})

	t.Run("tmux unavailable, node default names a session: bare fallback, no install attempt", func(t *testing.T) {
		t.Setenv("CITADEL_TMUX_BIN", filepath.Join(t.TempDir(), "missing"))
		installCalled := false
		cmd, _, wanted := resolveSessionCommand("citadel", "", "alice", "/bin/bash", func() bool {
			installCalled = true
			return true
		})
		if !wanted {
			t.Fatal("wantedSession = false, want true (node default names a session)")
		}
		if cmd != nil {
			t.Errorf("command = %v, want nil (bare fallback)", cmd)
		}
		if installCalled {
			t.Error("on-demand install must only fire for an explicit override (citadel #759), not the plain node default")
		}
	})

	t.Run("node default off", func(t *testing.T) {
		makeFakeTmux(t)
		cmd, _, wanted := resolveSessionCommand("none", "", "alice", "/bin/bash", nil)
		if wanted {
			t.Error("wantedSession = true, want false (node default is off)")
		}
		if cmd != nil {
			t.Errorf("command = %v, want nil", cmd)
		}
	})
}

// TestResolveSessionCommand_OverrideBareForcesBare locks in that a "none"
// override always wins, even when the node's own configured default names a
// persistent session; this is the actual #759 fix: the CLI path defaults
// to a bare shell regardless of the node's tmux-on-by-default posture.
func TestResolveSessionCommand_OverrideBareForcesBare(t *testing.T) {
	makeFakeTmux(t) // tmux IS available; the override must still force bare.
	cmd, name, wanted := resolveSessionCommand("citadel", "none", "alice", "/bin/bash", nil)
	if wanted {
		t.Error("wantedSession = true, want false (override forces bare)")
	}
	if cmd != nil {
		t.Errorf("command = %v, want nil (bare override)", cmd)
	}
	if name != "" {
		t.Errorf("sessionName = %q, want empty", name)
	}
}

// TestResolveSessionCommand_OverrideNamedRequestsTmux locks in --tmux's
// effect end to end: an override naming a session gets a real
// tmux-attach-or-create command when tmux resolves, even though the node's
// own default is "none" (an operator who disabled the persistent default
// can still have their OWN connection opt back in via --tmux).
func TestResolveSessionCommand_OverrideNamedRequestsTmux(t *testing.T) {
	bin := makeFakeTmux(t)
	cmd, name, wanted := resolveSessionCommand("none", "citadel", "alice", "/bin/bash", nil)
	if !wanted {
		t.Fatal("wantedSession = false, want true (override names a session)")
	}
	want := []string{bin, "new-session", "-A", "-s", name, "/bin/bash"}
	if !reflect.DeepEqual(cmd, want) {
		t.Errorf("command = %v, want %v", cmd, want)
	}
}

// TestResolveSessionCommand_OverrideTriggersInstallOnMissingTmux is the
// tmux-absent path: an override explicitly asking for a persistent session
// with no tmux binary resolvable attempts an on-node install (mirroring
// 'citadel tmux install') before falling back. A successful install must be
// picked up immediately (the fake makes tmux resolve as a side effect,
// simulating a real install landing a binary at the resolved path).
func TestResolveSessionCommand_OverrideTriggersInstallOnMissingTmux(t *testing.T) {
	t.Setenv("CITADEL_TMUX_BIN", filepath.Join(t.TempDir(), "missing"))

	var installCalls int
	installedBin := makeFakeTmuxPath(t)
	ensureInstall := func() bool {
		installCalls++
		// Simulate a successful install landing a binary CITADEL_TMUX_BIN can
		// now resolve.
		t.Setenv("CITADEL_TMUX_BIN", installedBin)
		return true
	}

	cmd, name, wanted := resolveSessionCommand("none", "citadel", "alice", "/bin/bash", ensureInstall)
	if !wanted {
		t.Fatal("wantedSession = false, want true (override names a session)")
	}
	if installCalls != 1 {
		t.Fatalf("ensureInstall called %d times, want 1", installCalls)
	}
	want := []string{installedBin, "new-session", "-A", "-s", name, "/bin/bash"}
	if !reflect.DeepEqual(cmd, want) {
		t.Errorf("command = %v, want %v (should resolve via the newly installed binary)", cmd, want)
	}
}

// TestResolveSessionCommand_OverrideInstallFailsFallsBackToBare pins the
// documented fallback: when the on-demand install ALSO fails (or the
// platform is gated/unsupported), the connection still succeeds as a bare
// shell rather than erroring out.
func TestResolveSessionCommand_OverrideInstallFailsFallsBackToBare(t *testing.T) {
	t.Setenv("CITADEL_TMUX_BIN", filepath.Join(t.TempDir(), "missing"))

	var installCalls int
	ensureInstall := func() bool {
		installCalls++
		return false // install attempted and failed
	}

	cmd, _, wanted := resolveSessionCommand("none", "citadel", "alice", "/bin/bash", ensureInstall)
	if !wanted {
		t.Fatal("wantedSession = false, want true (override names a session)")
	}
	if installCalls != 1 {
		t.Fatalf("ensureInstall called %d times, want 1", installCalls)
	}
	if cmd != nil {
		t.Errorf("command = %v, want nil (bare fallback after a failed install)", cmd)
	}
}

// makeFakeTmuxPath writes an executable tmux stub and returns its path
// WITHOUT pointing CITADEL_TMUX_BIN at it (unlike makeFakeTmux), so callers
// can simulate an install landing this exact binary at a controlled moment.
func makeFakeTmuxPath(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "tmux-installed")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("setup fake tmux: %v", err)
	}
	return bin
}
