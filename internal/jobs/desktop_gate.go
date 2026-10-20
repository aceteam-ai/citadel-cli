package jobs

// ReasonPasscodeUnavailable means the node could not read its own passcode
// state (permissions file or master-PIN vault), so a passcode-gated job is
// refused without judging the presented passcode.
const ReasonPasscodeUnavailable = "passcode_unavailable"

// msgDesktopPasscodeInvalid is the operator-facing message for a desktop job
// refused for a missing or wrong passcode.
const msgDesktopPasscodeInvalid = "desktop input or capture requires the node passcode; present it in the job payload `passcode` field"

const msgPasscodeUnavailable = "the node could not read its passcode configuration; passcode-gated jobs are refused until it can"

// PasscodeState is one read of the node's passcode configuration.
type PasscodeState struct {
	// Set reports whether a node passcode or master PIN is configured.
	Set bool
	// Verify checks a presented passcode against that configuration. A nil
	// Verify refuses.
	Verify func(pin string) bool
}

// DesktopGate is the per-node passcode gate for the screen/VNC/desktop job
// handlers (FILE_SCREENSHOT, VNC_SCREENSHOT, VNC_TYPE, VNC_KEYS, VNC_ACTIONS).
// It mirrors ShellGate's passcode half: the desktop permission itself is checked
// separately, by the adapter's live permission gate, before this runs.
//
// The passcode is presented in the job payload under ShellPasscodePayloadKey,
// the same wire key SHELL_COMMAND uses.
type DesktopGate struct {
	// Load reads the node's passcode state once per job. A node with no
	// passcode set runs desktop jobs unchanged, so a read failure must be
	// returned as an error, never as Set=false. A nil Load refuses.
	Load func() (PasscodeState, error)
}

// DesktopGateReason applies the gate to a presented passcode. It returns "" when
// the job may run, otherwise ReasonPasscodeUnavailable or
// ReasonPasscodeInvalid. A node with no passcode allows every desktop job, as
// before the gate existed.
func DesktopGateReason(g DesktopGate, presentedPasscode string) string {
	if g.Load == nil {
		return ReasonPasscodeUnavailable
	}
	state, err := g.Load()
	if err != nil {
		return ReasonPasscodeUnavailable
	}
	if !state.Set {
		return ""
	}
	if state.Verify == nil || !state.Verify(presentedPasscode) {
		return ReasonPasscodeInvalid
	}
	return ""
}

// DesktopRefusal builds the typed refusal for a desktop job that failed
// DesktopGateReason. It reuses ShellRefusal so the platform parses the same
// {"reason","message"} shape for every passcode-gated surface.
func DesktopRefusal(reason string) *ShellRefusal {
	if reason == ReasonPasscodeUnavailable {
		return &ShellRefusal{Reason: reason, Message: msgPasscodeUnavailable}
	}
	return &ShellRefusal{Reason: reason, Message: msgDesktopPasscodeInvalid}
}
