// cmd/connect_dispatch.go
//
// Shared bare-target dispatcher for `citadel ssh <node>` and
// `citadel connect <node>` (issue #754). A bare target (no :port) used to
// route straight through OpenSSH via a raw-TCP ProxyCommand to the peer's
// port 22, which embedded-tsnet nodes never expose on the mesh (only ports
// citadel explicitly ListenVPNs, namely 7860 terminal, 8080 status, 8444
// gateway, answer inbound; see cmd/mesh.go), so the dial was refused and
// ssh printed the opaque "Connection closed by UNKNOWN port 65535".
//
// Both commands now route a bare target through connectToNode, which:
//  1. tries the ts-net terminal endpoint first (runRemoteShell, :7860, the
//     path that already works on embedded-tsnet, since it rides a port
//     citadel does ListenVPN);
//  2. on a 401 that is specifically the terminal endpoint's optional
//     passcode gate (aceteam#6524/citadel#753), prompts interactively (no
//     echo) and retries ts-net. A passcode challenge means the endpoint IS
//     reachable, so it must never trigger the fallback below;
//  3. falls back to the legacy raw-sshd path (OpenSSH via
//     'citadel connect <ip>:22' as ProxyCommand) ONLY when the ts-net
//     endpoint itself is unreachable (connection refused / not listening).
//
// `citadel connect <node>:<port>` (an explicit port) is a separate,
// unchanged raw-TCP piping mode; see cmd/connect.go. It is never touched by
// any of this.
package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/terminal"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	// viaRaw forces the legacy raw-sshd path (OpenSSH via
	// 'citadel connect <ip>:22' as ProxyCommand), skipping the ts-net
	// terminal endpoint entirely. --via-sshd is an alias.
	viaRaw bool
	// viaMesh forces the ts-net terminal-endpoint path only: never falls
	// back to raw sshd even if the endpoint is unreachable. --terminal is an
	// alias.
	viaMesh bool
	// shellPasscode is the node passcode for the ts-net terminal endpoint's
	// optional passcode gate (aceteam#6524/citadel#753). Falls back to
	// CITADEL_TERMINAL_PASSCODE, then an interactive no-echo prompt on a
	// 401. Never logged, never placed on any subprocess argv.
	shellPasscode string
	// wantTmuxSession is set by --tmux: opt into a persistent, reconnect-
	// resilient tmux-backed session instead of the CLI's bare-shell default
	// (citadel #759).
	wantTmuxSession bool
	// wantNoTmuxSession is set by --no-tmux: explicit symmetry with --tmux.
	// It never changes behavior on its own (a bare shell is already the
	// default); it exists so operators can say what they mean and so
	// --tmux/--no-tmux together is a detectable, rejected conflict.
	wantNoTmuxSession bool
)

// maxPasscodeAttempts bounds the interactive retry loop so a mistyped
// passcode does not prompt forever, while still tolerating a typo.
const maxPasscodeAttempts = 3

// bareSessionOverride is the session-query value the terminal server
// (internal/terminal/server.go) reads as "force a bare, non-persistent
// shell regardless of the node's own default" (citadel #759). It is the
// CLI's default override, sent on every connect/ssh unless --tmux asks for
// a persistent session instead.
const bareSessionOverride = "none"

// rawFallbackNotice is printed right before falling back to the legacy raw
// sshd path. The guidance has to be up front, not appended after the fact:
// runLegacySSH calls os.Exit directly on an ssh(1) failure (preserving its
// exit code), so nothing printed after that point would ever be seen. If the
// node is embedded-tsnet with no host sshd exposed on the mesh (#754's
// original report), this fallback will ALSO fail, and the operator would
// otherwise be left with only OpenSSH's own opaque
// "Connection closed by UNKNOWN port 65535".
const rawFallbackNotice = "Terminal endpoint unreachable on the mesh; falling back to raw sshd (port 22). " +
	"If that also fails, this node likely does not expose host sshd on the mesh: " +
	"start 'citadel work' on it, or re-run with --mesh to see the terminal-endpoint error directly."

// terminalAttemptFn performs one attempt to open the ts-net terminal shell
// against target with the given passcode and session override. Package var
// so tests can substitute a fake without a live mesh/terminal server.
var terminalAttemptFn = runRemoteShell

// legacySSHFn execs the real ssh(1) binary against target via the raw-TCP
// ProxyCommand fallback. Package var so tests can substitute a fake without
// spawning a real ssh process.
var legacySSHFn = runLegacySSH

// promptPasscodeFn reads a node passcode from the terminal. Package var so
// tests can substitute a fixed answer without a TTY.
var promptPasscodeFn = promptPasscode

// connectToNode implements the shared bare-target routing for both
// `citadel ssh <node>` and `citadel connect <node>` (#754): both commands'
// Run functions call this with the raw target string, so they behave
// identically for the same target and flags.
func connectToNode(cmd *cobra.Command, target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ensureNetworkConnectedFn(ctx); err != nil {
		return err
	}

	// -u/--user and -p/--port only make sense against a real sshd, so naming
	// either implies the raw path: silently ignoring them on the ts-net path
	// would be a silent behavior regression for existing 'citadel ssh -p ...'
	// users. Only registered on sshCmd (see cmd/ssh.go), so this is always
	// false for a connectCmd invocation.
	portOrUserGiven := cmd.Flags().Changed("user") || cmd.Flags().Changed("port")

	if viaRaw && viaMesh {
		return fmt.Errorf("--raw/--via-sshd and --mesh/--terminal are mutually exclusive")
	}
	if portOrUserGiven && viaMesh {
		return fmt.Errorf("--user/--port select the raw sshd path, which conflicts with --mesh/--terminal (ts-net only, no sshd)")
	}
	if wantTmuxSession && wantNoTmuxSession {
		return fmt.Errorf("--tmux and --no-tmux are mutually exclusive")
	}

	if viaRaw || portOrUserGiven {
		return legacySSHFn(target)
	}

	passcode := shellPasscode
	if passcode == "" {
		passcode = os.Getenv("CITADEL_TERMINAL_PASSCODE")
	}

	// citadel #759: the CLI connect path always sends an explicit session
	// override, which is what makes 'citadel ssh'/'citadel connect' default
	// to a bare shell regardless of the node's own CITADEL_TERMINAL_SESSION
	// default (tmux ON by default, citadel #585); that default keeps
	// governing every OTHER caller (namely the web console), which never
	// sends this override at all. "none" forces a bare shell (the CLI
	// default, and also what --no-tmux asks for explicitly); --tmux instead
	// requests the node's standard persistent session name, so a reconnect
	// (with or without --tmux again) re-attaches to the same live shell.
	session := bareSessionOverride
	if wantTmuxSession {
		session = terminal.DefaultSessionName
	}

	for attempt := 1; attempt <= maxPasscodeAttempts; attempt++ {
		err := terminalAttemptFn(target, passcode, session)
		if err == nil {
			return nil
		}

		var tdErr *terminalDialError
		if errors.As(err, &tdErr) {
			switch tdErr.kind {
			case terminalDialErrPasscode:
				// A 401 from the passcode gate means the endpoint IS
				// reachable: never a fallback trigger (#754). Prompt for a
				// passcode and retry ts-net instead.
				if attempt == maxPasscodeAttempts {
					return fmt.Errorf("passcode rejected after %d attempt(s)", attempt)
				}
				warnColor.Fprintln(os.Stderr, "This node requires a passcode.")
				pc, perr := promptPasscodeFn("Passcode: ")
				if perr != nil {
					return fmt.Errorf("failed to read passcode: %w", perr)
				}
				passcode = pc
				continue
			case terminalDialErrUnreachable:
				if viaMesh {
					return err
				}
				warnColor.Fprintln(os.Stderr, rawFallbackNotice)
				return legacySSHFn(target)
			}
		}
		// Every other error (auth-rejected-but-not-passcode, 503, malformed
		// handshake, ...) is surfaced as-is: not a passcode-retry trigger,
		// not a fallback trigger.
		return err
	}
	return fmt.Errorf("passcode retry limit reached")
}

// promptPasscode reads a node passcode with terminal echo disabled where
// possible, so it never appears on screen, in shell history, or in any
// process argv.
func promptPasscode(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	stdinFd := int(os.Stdin.Fd())
	if term.IsTerminal(stdinFd) {
		b, err := term.ReadPassword(stdinFd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	// Non-interactive fallback (piped stdin, tests): there is no terminal to
	// suppress echo on anyway.
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
