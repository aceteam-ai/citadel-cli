// internal/servicediag/redact.go
package servicediag

import (
	"regexp"
	"sort"
	"strings"
)

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

// minSecretValueLenForTextRedaction is the shortest secret-shaped env value
// RedactText will scrub out of free text.
const minSecretValueLenForTextRedaction = 12

// looksLikeRandomSecretValue guards free-text scrubbing (RedactText) against
// dictionary-word / short-phrase / path env values that legitimately live
// under a secret-shaped KEY name but are not themselves secrets.
//
// This matters more here than it does for RedactEnv: in production,
// Diagnose's ResolvedEnv is the citadel PROCESS's full environment
// (cmd.composeEnv() folds in os.Environ()), not just what a compose file
// declares. secretKeyRe's key-name heuristic is a broad substring match
// (TOKEN/KEY/SECRET/PASSWORD/AUTH/CREDENTIAL), so free-text scrubbing runs
// against every matching var on the box -- things like
// TOKEN_TYPE=bearer, AUTH_MODE=disabled, API_KEY_HEADER=Authorization,
// XAUTHORITY=/run/user/1000/.Xauthority. For RedactEnv (ComposeInfo.Env,
// shown as key=value) blanking one of those is harmless: the key label is
// still visible, so nothing is hidden or misread. For free text (a log
// line, a rendered command) there is no label -- silently replacing an
// ordinary word like "bearer" or "Authorization" wherever it appears would
// corrupt the exact diagnostic text this command exists to show, and (since
// this runs before ExtractRootError/ErrorHints) could even suppress a hint
// match. So free-text matching additionally requires the value to actually
// look like a generated secret: no whitespace (a secret is one token, not a
// phrase or a path with spaces), and either long enough on its own (>=20,
// where a coincidental collision with unrelated log text becomes very
// unlikely) or containing both a letter and a digit at the minimum length
// (typical of API keys/tokens, and NOT typical of an English word).
func looksLikeRandomSecretValue(v string) bool {
	if len(v) < minSecretValueLenForTextRedaction {
		return false
	}
	if strings.ContainsAny(v, " \t\n\r") {
		return false
	}
	if len(v) >= 20 {
		return true
	}
	var hasDigit, hasLetter bool
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			hasLetter = true
		}
	}
	return hasDigit && hasLetter
}

// secretValuesForTextRedaction returns the secret-shaped (by key-name
// heuristic) values in env that also look like an actual generated secret
// (see looksLikeRandomSecretValue) and are therefore safe to match against
// free text, longest-first so a value that happens to be a substring of
// another secret value doesn't leave a partial leak after the first
// replacement.
func secretValuesForTextRedaction(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	var out []string
	for k, v := range env {
		if !looksLikeSecretKey(k) {
			continue
		}
		if !looksLikeRandomSecretValue(v) {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// RedactText replaces every verbatim occurrence of a secret-shaped env VALUE
// (from env, matched by value -- not by any key name in s itself) with
// redactedPlaceholder. It exists because a secret can leak into free text
// that carries no variable name to redact by: a compose `command:` that
// interpolates ${HF_TOKEN} renders the literal token into the command
// string, and application logs can echo a token/credentialed URL directly.
// A nil/empty env or s is returned unchanged.
func RedactText(s string, env map[string]string) string {
	if s == "" {
		return s
	}
	for _, v := range secretValuesForTextRedaction(env) {
		s = strings.ReplaceAll(s, v, redactedPlaceholder)
	}
	return s
}

// RedactLines applies RedactText to every line, returning a new slice (the
// input is never mutated in place). A nil/empty lines or env returns lines
// unchanged (by reference) since there is nothing to do.
func RedactLines(lines []string, env map[string]string) []string {
	if len(lines) == 0 || len(env) == 0 {
		return lines
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = RedactText(l, env)
	}
	return out
}
