// internal/servicediag/redact.go
package servicediag

import "regexp"

// secretKeyRe matches env var names that look like they hold a secret, by
// key-name heuristic only (never by inspecting the value): anything
// containing TOKEN, KEY, SECRET, PASSWORD, AUTH, or CREDENTIAL, case
// -insensitive. This intentionally over-redacts (e.g. "PUBKEY_PATH" gets
// redacted too) -- for a diagnostic printout, a false-positive redaction is
// the safe direction of error; a leaked secret is not.
var secretKeyRe = regexp.MustCompile(`(?i)(TOKEN|KEY|SECRET|PASSWORD|PASSWD|AUTH|CREDENTIAL)`)

// redactedPlaceholder is what a secret-shaped value is replaced with.
const redactedPlaceholder = "***REDACTED***"

// looksLikeSecretKey reports whether an env var name matches the secret-shaped
// heuristic.
func looksLikeSecretKey(key string) bool {
	return secretKeyRe.MatchString(key)
}

// RedactEnv returns a copy of env with every secret-shaped value (by key-name
// heuristic) replaced with redactedPlaceholder. A nil input returns nil.
func RedactEnv(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		if looksLikeSecretKey(k) {
			out[k] = redactedPlaceholder
		} else {
			out[k] = v
		}
	}
	return out
}
