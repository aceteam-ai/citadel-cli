package jobs

// msgDesktopPasscodeInvalid is the operator-facing message for a desktop job
// refused by DesktopGateReason.
const msgDesktopPasscodeInvalid = "desktop input or capture requires the node passcode; present it in the job payload `passcode` field"

// DesktopGate is the per-node passcode gate for the screen/VNC/desktop job
// handlers (FILE_SCREENSHOT, VNC_SCREENSHOT, VNC_TYPE, VNC_KEYS, VNC_ACTIONS).
// It mirrors ShellGate's passcode half: the desktop permission itself is checked
// separately, by the adapter's live permission gate, before this runs.
//
// The passcode is presented in the job payload under ShellPasscodePayloadKey,
// the same wire key SHELL_COMMAND uses.
type DesktopGate struct {
	// HasPasscode reports whether the node has a passcode configured. When it
	// reports false, desktop jobs run unchanged. A nil HasPasscode refuses
	// (fail closed): the gate cannot know whether a passcode is set.
	HasPasscode func() bool
	// VerifyPasscode checks a presented passcode against the node's passcode.
	// A nil VerifyPasscode refuses whenever a passcode is set (fail closed).
	VerifyPasscode func(pin string) bool
}

// DesktopGateReason applies the gate to a presented passcode. It returns "" when
// the job may run, otherwise ReasonPasscodeInvalid. A node with no passcode
// allows every desktop job, as before the gate existed.
func DesktopGateReason(g DesktopGate, presentedPasscode string) string {
	if g.HasPasscode == nil {
		return ReasonPasscodeInvalid
	}
	if !g.HasPasscode() {
		return ""
	}
	if g.VerifyPasscode == nil || !g.VerifyPasscode(presentedPasscode) {
		return ReasonPasscodeInvalid
	}
	return ""
}

// DesktopRefusal builds the typed refusal for a desktop job that failed
// DesktopGateReason. It reuses ShellRefusal so the platform parses the same
// {"reason","message"} shape for every passcode-gated surface.
func DesktopRefusal(reason string) *ShellRefusal {
	return &ShellRefusal{Reason: reason, Message: msgDesktopPasscodeInvalid}
}
