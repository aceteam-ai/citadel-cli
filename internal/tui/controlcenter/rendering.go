package controlcenter

import "os"

// tcellAltScreenEnv is the tcell environment variable that gates use of the
// terminal's alternate screen buffer. The vendored tcell reads it once, at
// screen Init() time (inside tview's Application.Run), in the unix tScreen's
// engage() path: when it equals "disable", tcell skips both entering
// (EnterCA) and exiting (ExitCA) the alternate screen, so output goes to the
// normal scrollback buffer instead of an app-like in-place repaint.
//
// This env var is the ONLY seam this tcell version exposes for the choice —
// there is no public setter on the screen — so a launch-time consumer must set
// it before Run() creates the screen. tview/tcell cannot swap the mode on a
// live screen, which is why the Settings toggle persists the choice and the
// apply happens at the next launch.
const tcellAltScreenEnv = "TCELL_ALTSCREEN"

// applyFullscreenRendering configures the process environment so the next tcell
// screen honors the fullscreen preference, and returns the value it set
// TCELL_ALTSCREEN to (empty string when unset) so callers can assert the effect
// without a terminal.
//
// It is symmetric on purpose: when fullscreen is enabled it UNSETS the variable
// (rather than merely skipping it) so a stale "disable" from an earlier context
// cannot leak the scrollback mode into a fullscreen launch. When disabled it
// sets "disable". This determinism is what makes the toggle round-trip
// correctly and keeps the helper unit-testable.
//
// Pure with respect to its input (the bool): it reads no config and holds no
// tview/tcell types, so the launch-time behavior can be exercised directly via
// os.Getenv in tests. The screen-creation coupling lives entirely in tcell's
// engage(); this helper only stages the flag it reads.
func applyFullscreenRendering(fullscreen bool) string {
	if fullscreen {
		_ = os.Unsetenv(tcellAltScreenEnv)
		return ""
	}
	_ = os.Setenv(tcellAltScreenEnv, "disable")
	return "disable"
}
