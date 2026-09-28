package jobs

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestTemplateJSONRejectsAmbiguity(t *testing.T) {
	for _, raw := range []string{`{"x":1,"x":2}`, `{"x":{"a":1,"a":2}}`, `{"x":1} {}`, `{"x":`, "{\"x\":\"\xff\"}", `"\ud800"`, `"\udc00"`, `"\ud800\u0061"`} {
		if _, err := DecodeTemplateJSON(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	for _, raw := range []string{`"\ud83d\ude00"`, `"\\ud800"`} {
		if _, err := DecodeTemplateJSON(json.RawMessage(raw)); err != nil {
			t.Errorf("valid unicode %s: %v", raw, err)
		}
	}
}

func TestTemplateRunnerClosedVocabulary(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"Kind":"builtin","handler":"h"}`, `{"kind":"builtin","Handler":"h"}`, `{"kind":"builtin","handler":"h","command":"evil"}`, `{"kind":" builtin","handler":"h"}`, `{"kind":"builtin","handler":"h "}`, `{"kind":"builtin","handler":1}`, `{"kind":"builtin","handler":"safe","handler":"evil"}`} {
		if _, err := ParseTemplateRunner(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	a, err := ParseTemplateRunner(json.RawMessage(`{"kind":"builtin","handler":"safe"}`))
	b, err2 := ParseTemplateRunner(json.RawMessage(`{"handler":"safe","kind":"builtin"}`))
	if err != nil || err2 != nil || a != b {
		t.Fatalf("canonical reordered runner differs: %v %v %v %v", a, b, err, err2)
	}
}

func TestTemplateSchemaLocalReferences(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","$defs":{"gain":{"type":"number","maximum":2}},"properties":{"gain":{"$ref":"#/$defs/gain"}},"required":["gain"]}`)
	if err := ValidateTemplateParams(json.RawMessage(`{"gain":1}`), schema, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTemplateParams(json.RawMessage(`{"gain":3}`), schema, json.RawMessage(`{}`)); err == nil {
		t.Fatal("local ref constraints were skipped")
	}
}

func TestTemplateSchemaRefusesExternalResources(t *testing.T) {
	for _, uri := range []string{"file:///etc/passwd", "http://127.0.0.1/schema.json", "https://example.com/schema.json"} {
		schema := json.RawMessage(`{"type":"object","$ref":"` + uri + `"}`)
		if err := ValidateTemplateParams(json.RawMessage(`{}`), schema, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "forbidden") {
			t.Fatalf("external schema %s: %v", uri, err)
		}
	}
	for _, schema := range []string{`null`, `[]`, `{"type":"bogus"}`, `{"type":"object","required":"gain"}`} {
		if err := ValidateTemplateParams(json.RawMessage(`{}`), json.RawMessage(schema), json.RawMessage(`{}`)); err == nil {
			t.Errorf("invalid schema accepted: %s", schema)
		}
	}
}

func TestTemplateInputReferenceValidation(t *testing.T) {
	for _, nodePath := range []string{"../secret", "/secret", "a/../b", "a//b", ".", "a\\b", "C:/secret"} {
		raw, _ := json.Marshal([]map[string]string{{"path": nodePath, "node_id": "n1", "node_path": nodePath}})
		if _, err := ParseTemplateInputFiles(raw, "n1"); err == nil {
			t.Errorf("unsafe node_path %q accepted", nodePath)
		}
	}
	if _, err := ParseTemplateInputFiles(json.RawMessage(`[{"path":"node:n1/a.txt","node_id":"n1","node_path":"a.txt"}]`), "n1"); err != nil {
		t.Fatal(err)
	}
}

// Values are json.dumps(json.loads(raw), separators=(",",":")) from CPython.
func TestTemplatePythonNumericCanonicalization(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"1e0", "1.0"}, {"1.00", "1.0"}, {"-0", "0"}, {"1E-6", "1e-06"},
		{"-0.0", "-0.0"}, {"0e0", "0.0"}, {"1e-4", "0.0001"}, {"1e-5", "1e-05"},
		{"1e15", "1000000000000000.0"}, {"1e16", "1e+16"}, {"1e20", "1e+20"},
		{"1.2345678901234567", "1.2345678901234567"},
		{"9007199254740993", "9007199254740993"}, {"9007199254740993.0", "9007199254740992.0"},
		{"5e-324", "5e-324"}, {"1e-400", "0.0"}, {"-1e-400", "-0.0"},
	} {
		v, err := DecodeTemplateJSON(json.RawMessage(tc.raw))
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if err := writeCanonicalJSON(&b, v); err != nil {
			t.Fatal(err)
		}
		if b.String() != tc.want {
			t.Errorf("%s: got %s, want %s", tc.raw, b.String(), tc.want)
		}
	}
	for _, raw := range []string{"1e400", "-1e400"} {
		var b bytes.Buffer
		if err := writeCanonicalJSON(&b, json.Number(raw)); err == nil {
			t.Errorf("non-finite number %s accepted", raw)
		}
	}
}
