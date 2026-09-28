package jobs

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestComputeTemplateManifestHash_MatchesPlatformGolden pins the Go
// recomputation byte-for-byte against values produced by the platform authority
// python-backend/utils/database/job_templates.py::compute_template_hash
// (json.dumps sort_keys=True, separators=(",",":"), ensure_ascii=True). If Go's
// canonicalization ever drifts from Python's, every legitimate run would
// falsely fail the anti-tamper check, so this must never be "fixed" by relaxing
// it to encoding/json defaults.
func TestComputeTemplateManifestHash_MatchesPlatformGolden(t *testing.T) {
	cases := []struct {
		name         string
		templateKey  string
		version      int
		inputSchema  string
		outputSchema string
		runner       string
		wantHash     string
	}{
		{
			name:         "ascii_manifest",
			templateKey:  "papercraft-render",
			version:      3,
			inputSchema:  `{"type":"object","properties":{"fps":{"type":"integer"},"title":{"type":"string"}},"required":["fps"]}`,
			outputSchema: `{"type":"object","properties":{"mp4":{"type":"string"}}}`,
			runner:       `{"kind":"builtin","handler":"papercraft-render"}`,
			wantHash:     "0432d6003d5419a7868af421931efb2e96bc09e338a163e464f14aff642018be",
		},
		{
			// Exercises the two ways Go's encoding/json differs from Python:
			// non-ASCII (é -> é under ensure_ascii) and the HTML chars
			// < and & (Python leaves them raw; json.Marshal would escape them).
			// Keys are out of order to prove sort_keys.
			name:         "ensure_ascii_and_raw_html_chars",
			templateKey:  "t&<x",
			version:      1,
			inputSchema:  `{"desc":"café < 5 & more","z":1,"a":2}`,
			outputSchema: `{}`,
			runner:       `{"kind":"builtin","handler":"audio-mix"}`,
			wantHash:     "fbea79bd7112e47939e1a04710d79c6d3f9155c8ea8ac8fc978eeb990232b3c1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ComputeTemplateManifestHash(
				tc.templateKey, tc.version,
				json.RawMessage(tc.inputSchema),
				json.RawMessage(tc.outputSchema),
				json.RawMessage(tc.runner),
			)
			if err != nil {
				t.Fatalf("computeTemplateManifestHash: %v", err)
			}
			if got != tc.wantHash {
				t.Errorf("hash = %s\n want %s", got, tc.wantHash)
			}
		})
	}
}

// TestComputeTemplateManifestHash_IntegerSchemaValuesNotFloats guards the
// UseNumber path: an integer schema constraint must canonicalize as "5", never
// "5.0" (which would diverge from Python and break the hash match).
func TestComputeTemplateManifestHash_IntegerSchemaValuesNotFloats(t *testing.T) {
	// Same as a plain-encoding/json round trip would differ on: 5 vs 5.0.
	withInt, err := ComputeTemplateManifestHash("k", 1,
		json.RawMessage(`{"minLength":5}`), json.RawMessage(`{}`),
		json.RawMessage(`{"kind":"builtin","handler":"h"}`))
	if err != nil {
		t.Fatal(err)
	}
	withFloatText, err := ComputeTemplateManifestHash("k", 1,
		json.RawMessage(`{"minLength":5.0}`), json.RawMessage(`{}`),
		json.RawMessage(`{"kind":"builtin","handler":"h"}`))
	if err != nil {
		t.Fatal(err)
	}
	if withInt == withFloatText {
		t.Fatal("expected 5 and 5.0 to canonicalize differently (json.Number preserves the literal form)")
	}
}

// TestWriteCanonicalString_PythonCompatible pins the string escaper against
// Python's py_encode_basestring_ascii, including a surrogate pair above the BMP.
func TestWriteCanonicalString_PythonCompatible(t *testing.T) {
	cases := []struct{ in, want string }{
		{"café < 5 & more", "\"caf\\u00e9 < 5 & more\""},
		{`a"b\c`, `"a\"b\\c"`},
		{"tab\tnew\nline\r", `"tab\tnew\nline\r"`},
		{"emoji\U0001F600!", "\"emoji\\ud83d\\ude00!\""},
		{"\x01\x1f", `"\u0001\u001f"`},
	}
	for _, tc := range cases {
		var b bytes.Buffer
		writeCanonicalString(&b, tc.in)
		if b.String() != tc.want {
			t.Errorf("writeCanonicalString(%q) = %s, want %s", tc.in, b.String(), tc.want)
		}
	}
}
