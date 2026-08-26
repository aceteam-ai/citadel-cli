package servicediag

import (
	"strings"
	"testing"
)

func TestRedactEnv(t *testing.T) {
	in := map[string]string{
		"API_KEY":       "sk-abc123",
		"AUTH_TOKEN":    "tok-xyz",
		"HF_TOKEN":      "hf_secret",
		"DB_PASSWORD":   "hunter2",
		"MY_SECRET_VAL": "s3cr3t",
		"CREDENTIAL_ID": "cred-1",
		"PORT":          "8213",
		"LOG_LEVEL":     "info",
		"MODEL_NAME":    "Qwen/Qwen3-8B",
	}
	out := RedactEnv(in)

	secretKeys := []string{"API_KEY", "AUTH_TOKEN", "HF_TOKEN", "DB_PASSWORD", "MY_SECRET_VAL", "CREDENTIAL_ID"}
	for _, k := range secretKeys {
		if out[k] != redactedPlaceholder {
			t.Errorf("out[%s] = %q, want redacted", k, out[k])
		}
	}

	plainKeys := map[string]string{"PORT": "8213", "LOG_LEVEL": "info", "MODEL_NAME": "Qwen/Qwen3-8B"}
	for k, want := range plainKeys {
		if out[k] != want {
			t.Errorf("out[%s] = %q, want unredacted %q", k, out[k], want)
		}
	}
}

func TestRedactEnv_Nil(t *testing.T) {
	if RedactEnv(nil) != nil {
		t.Error("RedactEnv(nil) should return nil")
	}
}

func TestRedactText_ScrubsSecretValueFromFreeText(t *testing.T) {
	env := map[string]string{"HF_TOKEN": "hf_supersecretvalue123"}
	s := "--host 0.0.0.0 --hf-token hf_supersecretvalue123 --port 8000"
	got := RedactText(s, env)
	if strings.Contains(got, "hf_supersecretvalue123") {
		t.Errorf("RedactText() = %q, still contains the secret value", got)
	}
	if !strings.Contains(got, redactedPlaceholder) {
		t.Errorf("RedactText() = %q, want it to contain %q", got, redactedPlaceholder)
	}
}

func TestRedactText_DoesNotOverRedactNonSecretText(t *testing.T) {
	// The asymmetry this fix must not break: ordinary diagnostic text (a
	// model name, a plain numeric, an arch-mismatch message) must survive
	// untouched. env carries a secret, but none of its VALUE appears in s, so
	// s must come back byte-for-byte identical.
	env := map[string]string{"HF_TOKEN": "hf_supersecretvalue123"}
	tests := []string{
		"ValueError: The checkpoint you are trying to load has model type `newmodel` but Transformers does not recognize this architecture.",
		"--host 0.0.0.0 --port 8000 --model Qwen/Qwen3-8B",
		"exit code 137",
	}
	for _, s := range tests {
		if got := RedactText(s, env); got != s {
			t.Errorf("RedactText(%q) = %q, want unchanged (no secret value present)", s, got)
		}
	}
}

func TestRedactText_SkipsShortSecretValues(t *testing.T) {
	// A short secret-shaped value is not scrubbed from free text -- too
	// likely to coincidentally match unrelated benign text (see
	// minSecretValueLenForTextRedaction's doc comment).
	env := map[string]string{"API_KEY": "on"}
	s := "flash attention is on for this engine"
	if got := RedactText(s, env); got != s {
		t.Errorf("RedactText(%q) = %q, want unchanged (secret value too short to scrub)", s, got)
	}
}

// TestRedactText_SkipsDictionaryWordishSecretShapedValues pins the fix for
// production ResolvedEnv being the citadel PROCESS's full environment
// (composeEnv() -> os.Environ()), not a curated compose-only set: a
// secret-shaped KEY name (matches TOKEN/AUTH/etc by substring) can carry an
// ordinary word/path value that is NOT a secret -- TOKEN_TYPE=bearer,
// AUTH_MODE=disabled, API_KEY_HEADER=Authorization. Scrubbing those out of
// free text would corrupt ordinary diagnostic text with no benefit.
func TestRedactText_SkipsDictionaryWordishSecretShapedValues(t *testing.T) {
	env := map[string]string{
		"TOKEN_TYPE":     "bearer",
		"AUTH_MODE":      "disabled",
		"API_KEY_HEADER": "Authorization", // 13 letters, no digit, len<20
	}
	s := "using bearer auth, mode disabled, header Authorization set"
	if got := RedactText(s, env); got != s {
		t.Errorf("RedactText(%q) = %q, want unchanged (values are not secret-shaped, just secret-KEYED)", s, got)
	}
}

// TestRedactText_ScrubsLongValueEvenWithoutDigit covers the >=20-length
// fallback: a long random-looking value with no digit (e.g. a base64/hex
// secret that happens to be all letters) must still be caught.
func TestRedactText_ScrubsLongValueEvenWithoutDigit(t *testing.T) {
	env := map[string]string{"API_KEY": "abcdefghijklmnopqrstuvwxyz"} // 26 letters, no digit
	s := "request failed with key abcdefghijklmnopqrstuvwxyz attached"
	got := RedactText(s, env)
	if strings.Contains(got, "abcdefghijklmnopqrstuvwxyz") {
		t.Errorf("RedactText() = %q, still contains the long secret-shaped value", got)
	}
}

// TestRedactText_SkipsValuesWithWhitespace covers a secret-shaped key whose
// value is a phrase or path with spaces -- not how generated secrets look,
// so it must not be treated as one for free-text scrubbing.
func TestRedactText_SkipsValuesWithWhitespace(t *testing.T) {
	env := map[string]string{"AUTH_NOTE": "temporarily disabled for testing 12345"}
	s := "config says: temporarily disabled for testing 12345"
	if got := RedactText(s, env); got != s {
		t.Errorf("RedactText(%q) = %q, want unchanged (value contains whitespace, not secret-shaped)", s, got)
	}
}

func TestRedactText_EmptyInputs(t *testing.T) {
	if got := RedactText("", map[string]string{"TOKEN": "abc123456"}); got != "" {
		t.Errorf("RedactText(\"\", ...) = %q, want empty", got)
	}
	s := "no secrets here"
	if got := RedactText(s, nil); got != s {
		t.Errorf("RedactText(s, nil) = %q, want unchanged %q", got, s)
	}
}

func TestRedactLines_ScrubsEachLine(t *testing.T) {
	env := map[string]string{"HF_TOKEN": "hf_supersecretvalue123"}
	lines := []string{
		"INFO: loading with token hf_supersecretvalue123",
		"INFO: model ready",
	}
	got := RedactLines(lines, env)
	if strings.Contains(got[0], "hf_supersecretvalue123") {
		t.Errorf("RedactLines()[0] = %q, still contains the secret value", got[0])
	}
	if got[1] != "INFO: model ready" {
		t.Errorf("RedactLines()[1] = %q, want unchanged", got[1])
	}
}

func TestRedactLines_EmptyInputs(t *testing.T) {
	if got := RedactLines(nil, map[string]string{"TOKEN": "abc123456"}); got != nil {
		t.Errorf("RedactLines(nil, ...) = %v, want nil", got)
	}
	lines := []string{"a line"}
	if got := RedactLines(lines, nil); len(got) != 1 || got[0] != "a line" {
		t.Errorf("RedactLines(lines, nil) = %v, want unchanged", got)
	}
}
