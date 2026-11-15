package jobprivacy

// PasscodeKey is the job payload field carrying the node passcode
// (jobs.ShellPasscodePayloadKey). It authorizes one job and is never retained.
const PasscodeKey = "passcode"

// PINKey is the COBROWSE_SESSION start field carrying the secret that unlocks
// a persistent profile. Since #820 that secret must be an at-rest passphrase,
// so it is treated exactly like the passcode: never retained.
const PINKey = "pin"

// secretKeys are the payload fields dropped from every retained copy (DLQ
// entries, logs, Job JSON and fmt renderings), for every job type.
var secretKeys = [...]string{PasscodeKey, PINKey}

// HasPasscode reports whether payload carries a passcode or pin field.
func HasPasscode(payload map[string]any) bool {
	for _, k := range secretKeys {
		if _, ok := payload[k]; ok {
			return true
		}
	}
	return false
}

// WithoutPasscode returns payload without its passcode and pin fields. The
// input is not modified; a payload with neither is returned unchanged.
func WithoutPasscode(payload map[string]any) map[string]any {
	if !HasPasscode(payload) {
		return payload
	}
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		if !isSecretKey(k) {
			out[k] = v
		}
	}
	return out
}

func isSecretKey(k string) bool {
	for _, s := range secretKeys {
		if k == s {
			return true
		}
	}
	return false
}

// redactPasscode removes a reflected passcode or pin value from text.
func redactPasscode(payload map[string]any, text string) string {
	for _, k := range secretKeys {
		if v, ok := payload[k].(string); ok && v != "" {
			text = replaceAll(text, v)
		}
	}
	return text
}
