//go:build windows

package session

import (
	"golang.org/x/sys/windows"
)

// detectDesktop probes for interactive desktop access on Windows. A process
// running in Session 0 (the isolated services session) or in a session other
// than the active physical console has no access to the interactive desktop
// (WinSta0\Default) and cannot drive VNC/screenshot/input.
//
// CGO-free: uses golang.org/x/sys/windows WTSGetActiveConsoleSessionId and
// ProcessIdToSessionId.
func detectDesktop() *DesktopInfo {
	consoleSession := windows.WTSGetActiveConsoleSessionId()

	var procSession uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &procSession); err != nil {
		// If we cannot resolve our own session, conservatively report headless.
		return &DesktopInfo{
			HasDesktop:  false,
			SessionType: "unknown",
			Reason:      "could not resolve process session id: " + err.Error(),
		}
	}
	return evaluateWindows(procSession, consoleSession)
}
