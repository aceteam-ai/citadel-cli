package jobprivacy

// PasscodeKey is the job payload field carrying the node passcode
// (jobs.ShellPasscodePayloadKey). It authorizes one job and is never retained.
const PasscodeKey = "passcode"

// HasPasscode reports whether payload carries a passcode field.
func HasPasscode(payload map[string]any) bool {
	_, ok := payload[PasscodeKey]
	return ok
}

// WithoutPasscode returns payload without the passcode field. The input is not
// modified; a payload with no passcode is returned unchanged.
func WithoutPasscode(payload map[string]any) map[string]any {
	if !HasPasscode(payload) {
		return payload
	}
	out := make(map[string]any, len(payload)-1)
	for k, v := range payload {
		if k != PasscodeKey {
			out[k] = v
		}
	}
	return out
}

// redactPasscode removes a reflected passcode value from text.
func redactPasscode(payload map[string]any, text string) string {
	if pin, ok := payload[PasscodeKey].(string); ok && pin != "" {
		return replaceAll(text, pin)
	}
	return text
}
