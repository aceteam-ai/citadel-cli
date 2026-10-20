package definition

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gowebpki/jcs"
)

func sharedDefinition(t testing.TB) (string, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", "contracts", "definition-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		OrganizationID string          `json:"organization_id"`
		Definition     json.RawMessage `json:"definition"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture.OrganizationID, fixture.Definition
}

func TestSharedDefinitionVector(t *testing.T) {
	org, data := sharedDefinition(t)
	canonical, err := Canonical(org, data)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := Digest(org, data)
	if err != nil {
		t.Fatal(err)
	}
	if digest != sharedExpectedDigest(t, "base") {
		t.Fatalf("shared definition digest changed: %s", digest)
	}
	t.Logf("canonical=%s\ndigest=%s", canonical, digest)
	// Reformatting wire JSON does not change the definition digest.
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, data, "", "  "); err != nil {
		t.Fatal(err)
	}
	if got, err := Digest(org, formatted.Bytes()); err != nil || got != digest {
		t.Fatal("wire whitespace changed the digest")
	}
	if other, err := Digest("00000000-0000-4000-8000-000000000004", data); err != nil || other == digest {
		t.Fatal("definition digest lost organization binding")
	}
}

func TestDefinitionRefusesAmbiguousAndUnmaterializedData(t *testing.T) {
	org, original := sharedDefinition(t)
	for _, tc := range []struct{ name, old, replacement string }{
		{"unknown top field", `"version": 1`, `"version": 1, "status": "active"`},
		{"case alias", `"version": 1`, `"Version": 1`},
		{"duplicate key", `"version": 1`, `"version": 1, "version": 2`},
		{"escaped duplicate", `"version": 1`, `"version": 1, "\u0076ersion": 2`},
		{"coercion", `"version": 1`, `"version": "1"`},
		{"float integer", `"version": 1`, `"version": 1.0`},
		{"exponent integer", `"version": 1`, `"version": 1e0`},
		{"unsafe integer", `"version": 1`, `"version": 9007199254740992`},
		{"negative zero", `"max_lines": 0`, `"max_lines": -0`},
		{"missing explicit null", `"bind": null,`, ``},
		{"old floating cost name", `"max_run_cost_micro_usd": 1000000`, `"max_run_cost_usd": 1`},
		{"fractional micro cost", `"max_run_cost_micro_usd": 1000000`, `"max_run_cost_micro_usd": 0.5`},
		{"overflow cost", `"daily_cap_micro_usd": 5000000`, `"daily_cap_micro_usd": 5000001`},
		{"invalid surrogate", `"logs": "$logs"`, `"\ud800": "$logs"`},
		{"high then non-low surrogate", `"logs": "$logs"`, `"\ud800\u0041": "$logs"`},
		{"high then high surrogate", `"logs": "$logs"`, `"\ud800\ud800": "$logs"`},
		{"low then high surrogate", `"logs": "$logs"`, `"\udc00\ud800": "$logs"`},
		{"low then low surrogate", `"logs": "$logs"`, `"\udc00\udc00": "$logs"`},
		{"path traversal", `/tmp/synthetic-stream/auth.log`, `/tmp/../auth.log`},
		{"node overflow", `"node_id": "1297"`, `"node_id": "18446744073709551616"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := strings.Replace(string(original), tc.old, tc.replacement, 1)
			if mutated == string(original) {
				t.Fatal("test mutation did not apply")
			}
			if _, err := Digest(org, []byte(mutated)); err == nil {
				t.Fatal("ambiguous or unsupported wire definition accepted")
			}
		})
	}
	for _, invalid := range []string{"{}", "null", "[]", string(original) + " {}", strings.Repeat("[", 17) + strings.Repeat("]", 17), strings.Repeat(" ", MaxBytes+1), string([]byte{0xff})} {
		if _, err := Digest(org, []byte(invalid)); err == nil {
			t.Fatal("invalid bounded object accepted")
		}
	}
}

func TestDefinitionPreservesLiteralBackslashUnicodeText(t *testing.T) {
	org, original := sharedDefinition(t)
	modified := strings.Replace(string(original), `"logs": "$logs"`, `"literal\\ud800\\u0041": "$logs"`, 1)
	if _, err := Digest(org, []byte(modified)); err != nil {
		t.Fatal("literal backslash-u text is not a Unicode escape", err)
	}
}

func TestSharedMalformedUTF16Vectors(t *testing.T) {
	org, original := sharedDefinition(t)
	data, err := os.ReadFile(filepath.Join("..", "testdata", "contracts", "definition-v1-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Keys []string `json:"invalid_utf16_keys"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil || len(vectors.Keys) != 5 {
		t.Fatal("missing malformed UTF16 vectors")
	}
	for _, rawKey := range vectors.Keys {
		modified := strings.Replace(string(original), `"logs": "$logs"`, rawKey+`: "$logs"`, 1)
		if _, err := Digest(org, []byte(modified)); err == nil {
			t.Fatal("shared malformed UTF16 vector accepted", rawKey)
		}
	}
}

func TestDefinitionCanonicalizationUnicodeEscapingAndIntegerEdges(t *testing.T) {
	org, original := sharedDefinition(t)
	modified := sharedEdgeDefinition(t, original)
	canonical, err := Canonical(org, modified)
	if err != nil {
		t.Fatal(err)
	}
	// JCS sorts object names by UTF-16, not Go UTF-8 byte order. Combining
	// sequences are not NFC-normalized; HTML characters stay literal.
	if !bytes.Contains(canonical, []byte(`"input_map":{"é":"$logs","é<\n":"$logs","𐀀":"$alert","":"$alert"}`)) || !bytes.Contains(canonical, []byte(`"version":9007199254740991`)) {
		t.Fatalf("JCS edge bytes diverged: %s", canonical)
	}
	if again, err := jcs.Transform(canonical); err != nil || !bytes.Equal(again, canonical) {
		t.Fatal("JCS is not idempotent")
	}
	digest, err := Digest(org, modified)
	if err != nil {
		t.Fatal(err)
	}
	if digest != sharedExpectedDigest(t, "unicode_integer_edge") {
		t.Fatalf("shared Unicode/number-edge digest changed: %s", digest)
	}
	t.Logf("edge canonical=%s\nedge digest=%s", canonical, digest)
}

func sharedEdgeDefinition(t testing.TB, original []byte) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", "contracts", "definition-v1-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Edge struct {
			Replace map[string]json.RawMessage `json:"replace_definition_members"`
		} `json:"unicode_integer_edge"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	var definition, escalate map[string]json.RawMessage
	if err := json.Unmarshal(original, &definition); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(definition["escalate"], &escalate); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Edge.Replace) != 2 {
		t.Fatal("shared edge must replace exactly two definition members")
	}
	definition["version"] = vectors.Edge.Replace["version"]
	escalate["input_map"] = vectors.Edge.Replace["escalate.input_map"]
	definition["escalate"], err = json.Marshal(escalate)
	if err != nil {
		t.Fatal(err)
	}
	modified, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	return modified
}

func sharedExpectedDigest(t testing.TB, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", "contracts", "definition-v1-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]json.RawMessage
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	var vector struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(fixtures[name], &vector); err != nil || !digestString(vector.Digest) {
		t.Fatal("invalid shared expected digest")
	}
	return vector.Digest
}
