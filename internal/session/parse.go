package session

import (
	"bufio"
	"fmt"
	"strings"
)

// linuxEnv captures the environment inputs the Linux probe depends on.
type linuxEnv struct {
	display        string
	waylandDisplay string
	xdgSessionType string
}

// evaluateLinux is the pure decision function for Linux desktop detection.
func evaluateLinux(env linuxEnv) *DesktopInfo {
	switch {
	case env.waylandDisplay != "":
		return &DesktopInfo{
			HasDesktop:  true,
			SessionType: "wayland",
			Reason:      "WAYLAND_DISPLAY=" + env.waylandDisplay,
		}
	case env.display != "":
		return &DesktopInfo{
			HasDesktop:  true,
			SessionType: "x11",
			Reason:      "DISPLAY=" + env.display,
		}
	default:
		st := "headless"
		if env.xdgSessionType != "" && env.xdgSessionType != "unspecified" {
			st = env.xdgSessionType
		}
		return &DesktopInfo{
			HasDesktop:  false,
			SessionType: st,
			Reason:      "headless session (no DISPLAY/WAYLAND_DISPLAY)",
		}
	}
}

// darwinEnv captures the env inputs the macOS probe depends on.
type darwinEnv struct {
	sshTTY        string
	sshConnection string
}

// evaluateDarwin is the pure decision function for macOS desktop detection.
// consoleUser is the username owning the GUI console session (empty if none).
func evaluateDarwin(env darwinEnv, consoleUser string) *DesktopInfo {
	if env.sshTTY != "" || env.sshConnection != "" {
		return &DesktopInfo{
			HasDesktop:  false,
			SessionType: "ssh",
			Reason:      "running over SSH (no Aqua GUI session)",
		}
	}
	if consoleUser == "" || consoleUser == "loginwindow" {
		return &DesktopInfo{
			HasDesktop:  false,
			SessionType: "headless",
			Reason:      "no logged-in console GUI user",
		}
	}
	return &DesktopInfo{
		HasDesktop:  true,
		SessionType: "aqua",
		Reason:      "Aqua GUI session (console user " + consoleUser + ")",
	}
}

// evaluateWindows is the pure decision function for Windows desktop detection.
// procSession is the session the process runs in; consoleSession is the active
// physical console session (0xFFFFFFFF when no one is attached).
func evaluateWindows(procSession, consoleSession uint32) *DesktopInfo {
	const noActiveConsole = 0xFFFFFFFF

	switch {
	case procSession == 0:
		return &DesktopInfo{
			HasDesktop:  false,
			SessionType: "session0",
			Reason:      "Session 0 isolation (service session has no interactive desktop)",
		}
	case consoleSession == noActiveConsole:
		return &DesktopInfo{
			HasDesktop:  false,
			SessionType: "headless",
			Reason:      "no active console session attached",
		}
	case procSession != consoleSession:
		return &DesktopInfo{
			HasDesktop:  false,
			SessionType: "disconnected",
			Reason: fmt.Sprintf("process session %d is not the active console session %d",
				procSession, consoleSession),
		}
	default:
		return &DesktopInfo{
			HasDesktop:  true,
			SessionType: "console",
			Reason:      fmt.Sprintf("interactive console session %d", procSession),
		}
	}
}

// parseConsoleUser extracts the console user name from `scutil show
// State:/Users/ConsoleUser` output. The output is a SystemConfiguration
// dictionary dump, e.g.:
//
//	<dictionary> {
//	  GID : 20
//	  Name : jason
//	  UID : 501
//	}
//
// Returns the value of the "Name" key, or "" if absent. When no user is logged
// in the dictionary is empty (or Name is "loginwindow"); both cases are handled
// by the caller. This helper is pure and unit-testable on any platform.
func parseConsoleUser(output string) string {
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "Name") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "Name"))
		if !strings.HasPrefix(rest, ":") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(rest, ":"))
		return name
	}
	return ""
}
