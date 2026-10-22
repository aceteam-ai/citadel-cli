// cmd/network_logf_test.go
package cmd

import (
	"reflect"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/tmux"
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
	resetBareDispatchState(t)
	bareStdoutIsTTYFn = func() bool { return false }
	bareStdinIsTTYFn = func() bool { return false }
	if rootCmd.PersistentPreRun == nil {
		t.Fatal("rootCmd has no PersistentPreRun, so nothing wires the network logger")
	}

	rootCmd.PersistentPreRun(rootCmd, nil)

	if !network.LogfConfigured() {
		t.Error("PersistentPreRun must install a real network diagnostic logger; " +
			"without it every command silently discards engine diagnostics (#662)")
	}
	if !tmux.LogfConfigured() {
		t.Error("PersistentPreRun must install a real tmux diagnostic logger; " +
			"without it scope/fallback decisions are silently discarded (#1174)")
	}
}

// TestNoSubcommandShadowsRootPersistentPreRun guards the trap that would undo
// the fix silently.
//
// Cobra runs only the CLOSEST PersistentPreRun in the chain, not all of them. A
// subcommand that grows its own would stop inheriting the root's wiring and go
// back to discarding diagnostics -- with no compile error and no test failure
// anywhere else. The three declared-session entrypoints are intentionally local
// and must not run root's credential, CA or updater startup. Their exact named
// hooks are checked separately, including legacy attach delegation. No other
// hook, error hook, nested command or same-name replacement gets this exception.
func TestNoSubcommandShadowsRootPersistentPreRun(t *testing.T) {
	var shadowed []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if (sub.PersistentPreRun != nil || sub.PersistentPreRunE != nil) &&
				!auditedDeclaredStartupHook(rootCmd, sub) {
				shadowed = append(shadowed, sub.CommandPath())
			}
			walk(sub)
		}
	}
	walk(rootCmd)

	if len(shadowed) > 0 {
		t.Errorf("these commands define their own PersistentPreRun, which shadows the root hook "+
			"and drops the network-logf wiring (#662): %s\n"+
			"Only the exact audited declared-session startup hooks are exempt.",
			strings.Join(shadowed, ", "))
	}
}

func auditedDeclaredStartupHook(root, command *cobra.Command) bool {
	if command.Parent() != root || command.PersistentPreRun == nil || command.PersistentPreRunE != nil {
		return false
	}
	want := declaredLocalOnly
	switch command.Name() {
	case "sessions", "session-host":
	case "attach":
		want = declaredAttachPreRun
	default:
		return false
	}
	return reflect.ValueOf(command.PersistentPreRun).Pointer() == reflect.ValueOf(want).Pointer()
}

func TestDeclaredStartupExceptionRejectsOtherHooks(t *testing.T) {
	for _, test := range []struct {
		name    string
		hook    func(*cobra.Command, []string)
		errHook bool
		nested  bool
		want    bool
	}{
		{name: "sessions", hook: declaredLocalOnly, want: true},
		{name: "session-host", hook: declaredLocalOnly, want: true},
		{name: "attach", hook: declaredAttachPreRun, want: true},
		{name: "other", hook: declaredLocalOnly},
		{name: "sessions"},
		{name: "sessions", hook: func(*cobra.Command, []string) {}},
		{name: "attach", hook: declaredLocalOnly},
		{name: "sessions", hook: declaredAttachPreRun},
		{name: "sessions", hook: declaredLocalOnly, errHook: true},
		{name: "sessions", hook: declaredLocalOnly, nested: true},
	} {
		root := &cobra.Command{Use: "fixture"}
		parent := root
		if test.nested {
			parent = &cobra.Command{Use: "nested"}
			root.AddCommand(parent)
		}
		command := &cobra.Command{Use: test.name, PersistentPreRun: test.hook}
		if test.errHook {
			command.PersistentPreRunE = func(*cobra.Command, []string) error { return nil }
		}
		parent.AddCommand(command)
		if got := auditedDeclaredStartupHook(root, command); got != test.want {
			t.Fatalf("name=%s nested=%v error-hook=%v got=%v want=%v", test.name, test.nested, test.errHook, got, test.want)
		}
	}
}
