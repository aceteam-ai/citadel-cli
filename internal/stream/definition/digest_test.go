package definition

import (
	"bytes"
	"encoding/hex"
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
	data, err := os.ReadFile(filepath.Join("..", "testdata", "contracts", "definition-v1-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Negative []struct {
			ID        string `json:"id"`
			Operation string `json:"operation"`
			Old       string `json:"old"`
			New       string `json:"new"`
			Text      string `json:"text"`
			Depth     int    `json:"depth"`
			Bytes     int    `json:"bytes"`
			Hex       string `json:"hex"`
		} `json:"negative_definitions"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil || len(vectors.Negative) != 27 {
		t.Fatal("missing shared definition refusal vectors", err)
	}
	seen := map[string]bool{}
	for _, vector := range vectors.Negative {
		if vector.ID == "" || seen[vector.ID] {
			t.Fatal("duplicate or empty vector id")
		}
		seen[vector.ID] = true
		t.Run(vector.ID, func(t *testing.T) {
			var mutated []byte
			switch vector.Operation {
			case "replace_first":
				if vector.Old == "" || strings.Count(string(original), vector.Old) != 1 {
					t.Fatal("mutation must target exactly one base fixture occurrence")
				}
				mutated = []byte(strings.Replace(string(original), vector.Old, vector.New, 1))
			case "raw_text":
				mutated = []byte(vector.Text)
			case "append_text":
				mutated = append(append([]byte(nil), original...), []byte(vector.Text)...)
			case "nested_arrays":
				mutated = []byte(strings.Repeat("[", vector.Depth) + strings.Repeat("]", vector.Depth))
			case "pad_spaces":
				if vector.Bytes <= len(original) {
					t.Fatal("padded vector must extend the original valid object")
				}
				mutated = append(append([]byte(nil), original...), bytes.Repeat([]byte{' '}, vector.Bytes-len(original))...)
				if !json.Valid(mutated) {
					t.Fatal("byte-limit vector must remain valid JSON")
				}
			case "raw_hex":
				var err error
				mutated, err = hex.DecodeString(vector.Hex)
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("unknown shared definition operation", vector.Operation)
			}
			if bytes.Equal(mutated, original) {
				t.Fatal("test mutation did not apply")
			}
			if _, err := Digest(org, mutated); err == nil {
				t.Fatal("shared negative definition accepted")
			}
		})
	}
}

func TestDefinitionByteLimitAcceptsBoundaryAndRefusesOneMore(t *testing.T) {
	org, original := sharedDefinition(t)
	padded := append(append([]byte(nil), original...), bytes.Repeat([]byte{' '}, MaxBytes-len(original))...)
	if got, err := Digest(org, padded); err != nil || got != sharedExpectedDigest(t, "base") {
		t.Fatal("valid definition at exact byte limit must preserve its digest", err)
	}
	if _, err := Digest(org, append(padded, ' ')); err == nil {
		t.Fatal("valid JSON beyond byte limit was accepted")
	}
}

func TestSharedDefinitionStructuralBoundaries(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "testdata", "contracts", "definition-v1-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Cases []struct {
			ID        string `json:"id"`
			Operation string `json:"operation"`
			Depth     int    `json:"depth"`
			Count     int    `json:"count"`
			Accept    bool   `json:"accept"`
		} `json:"structural_json"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil || len(vectors.Cases) != 4 {
		t.Fatal("missing structural boundary vectors", err)
	}
	for _, vector := range vectors.Cases {
		t.Run(vector.ID, func(t *testing.T) {
			var raw []byte
			switch vector.Operation {
			case "nested_arrays":
				raw = []byte(strings.Repeat("[", vector.Depth) + strings.Repeat("]", vector.Depth))
			case "zero_array":
				raw = []byte("[" + strings.Repeat("0,", vector.Count-1) + "0]")
			default:
				t.Fatal("unknown structural operation", vector.Operation)
			}
			if !json.Valid(raw) {
				t.Fatal("boundary vector must be syntactically valid JSON")
			}
			if got := boundedJSON(raw); got != vector.Accept {
				t.Fatalf("structural admission=%v, want %v", got, vector.Accept)
			}
		})
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
