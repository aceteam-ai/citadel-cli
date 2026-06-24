//go:build darwin

package session

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// stringReader adapts a string to an io.Reader for command stdin.
func stringReader(s string) *strings.Reader { return strings.NewReader(s) }

// detectDesktop probes for a logged-in Aqua GUI session on macOS. A process
// launched over SSH (SSH_TTY / SSH_CONNECTION set) has no window server, so it
// is reported as headless even if a user is logged in at the physical console.
//
// CGO-free: uses env checks plus a `scutil`/`stat` subprocess to confirm a
// console GUI user is present.
func detectDesktop() *DesktopInfo {
	env := darwinEnv{
		sshTTY:        os.Getenv("SSH_TTY"),
		sshConnection: os.Getenv("SSH_CONNECTION"),
	}
	consoleUser := detectConsoleUser()
	return evaluateDarwin(env, consoleUser)
}

// detectConsoleUser returns the username of the current GUI console session, or
// "" if none. It queries SystemConfiguration via scutil, which reports the
// active console user (or "loginwindow" when sitting at the login screen).
func detectConsoleUser() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// `scutil` reads the dynamic store; the GUIConsoleUser script returns the
	// console user name on stdout. This is CGO-free and present on all macOS.
	cmd := exec.CommandContext(ctx, "/usr/sbin/scutil")
	cmd.Stdin = stringReader("show State:/Users/ConsoleUser\n")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return parseConsoleUser(string(out))
}
