package trust

import (
	"encoding/base64"
	"testing"
)

// TestCanonicalJSON_MatchesPythonEnsureAscii pins the canonical_json port
// against the EXACT bytes aceteam-aep's canonical_json produces for an
// adversarial string exercising every escaping branch: the short escapes
// (\" \\ \n \r \t \b \f), other control chars and DEL as \u00XX, the
// deliberately-NOT-HTML-escaped < > & /, a non-ASCII BMP char (é -> é),
// and a non-BMP char (😀 -> the UTF-16 surrogate pair 😀). The
// expected value below was produced by running the real
// aceteam_aep.attestation.canonical_json({"s": adv}) during development.
func TestCanonicalJSON_MatchesPythonEnsureAscii(t *testing.T) {
	adv := "a\"b\\c\n\r\t\b\f\x01\x1f\x7f<>&/é\U0001F600"

	// base64 of the real Python canonical_json({"s": adv}) output.
	const wantB64 = "eyJzIjoiYVwiYlxcY1xuXHJcdFxiXGZcdTAwMDFcdTAwMWZcdTAwN2Y8PiYvXHUwMGU5XHVkODNkXHVkZTAwIn0="
	want, err := base64.StdEncoding.DecodeString(wantB64)
	if err != nil {
		t.Fatal(err)
	}

	got, err := CanonicalJSON(map[string]any{"s": adv})
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("canonical_json escaping mismatch:\n got %q\nwant %q", got, want)
	}
}

// TestCanonicalJSON_SortsKeysAtEveryDepthAndDropsNil pins the structural rules:
// keys sorted at every depth, compact separators, nil dropped from maps but
// null kept in lists.
func TestCanonicalJSON_SortsKeysAtEveryDepthAndDropsNil(t *testing.T) {
	in := map[string]any{
		"b": 2,
		"a": map[string]any{"z": true, "y": nil, "x": "v"},
		"c": []any{1, nil, "three"},
	}
	got, err := CanonicalJSON(in)
	if err != nil {
		t.Fatal(err)
	}
	// "a" sorted first, its nil "y" dropped, "x" before "z"; list keeps null.
	want := `{"a":{"x":"v","z":true},"b":2,"c":[1,null,"three"]}`
	if string(got) != want {
		t.Errorf("CanonicalJSON:\n got %q\nwant %q", got, want)
	}
}

// TestCanonicalJSON_RejectsFloat64 pins the guardrail: the verdict preimage
// carries score as a string, so any float64 reaching the encoder is a bug and
// is rejected rather than rendered (Go and Python disagree on float formatting).
func TestCanonicalJSON_RejectsFloat64(t *testing.T) {
	if _, err := CanonicalJSON(map[string]any{"score": 0.5}); err == nil {
		t.Error("CanonicalJSON accepted a float64; want a rejection")
	}
}

// TestVerdictHashPreimage_ScoreIsFixedString pins that the preimage carries
// score as the FormatFloat 'f' 6 string, not a float (design §3.4 option A).
func TestVerdictHashPreimage_ScoreIsFixedString(t *testing.T) {
	p := VerdictHashPreimage("flag", nil, GroundingResult{Grounded: false, Score: 0.5, ClaimsChecked: 2}, "sha256:pol")
	g, ok := p["grounding"].(map[string]any)
	if !ok {
		t.Fatalf("preimage grounding = %#v, want map", p["grounding"])
	}
	if g["score"] != "0.500000" {
		t.Errorf(`preimage grounding.score = %#v, want the string "0.500000"`, g["score"])
	}
}
