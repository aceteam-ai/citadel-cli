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
	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"$defs":{"gain":{"type":"number","maximum":2}},"properties":{"gain":{"$ref":"#/$defs/gain"}},"required":["gain"]}`)
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

func TestTemplateSchemaUsesHashedNumericSemantics(t *testing.T) {
	for _, tc := range []struct {
		schema, params string
		accept         bool
	}{
		{`{"type":"object","additionalProperties":false,"properties":{"gain":{"maximum":1e-400}}}`, `{"gain":1e-300}`, false},
		{`{"type":"object","additionalProperties":false,"properties":{"gain":{"minimum":1e-400}}}`, `{"gain":0}`, true},
		{`{"type":"object","additionalProperties":false,"properties":{"gain":{"maximum":9007199254740993.0}}}`, `{"gain":9007199254740993}`, false},
		{`{"type":"object","additionalProperties":false,"properties":{"gain":{"const":0.0}}}`, `{"gain":1e-400}`, true},
	} {
		err := ValidateTemplateParams(json.RawMessage(tc.params), json.RawMessage(tc.schema), json.RawMessage(`{}`))
		if (err == nil) != tc.accept {
			t.Errorf("schema %s params %s: accept=%v err=%v", tc.schema, tc.params, tc.accept, err)
		}
	}
	got, err := NormalizeTemplateParams(json.RawMessage(`{"gain":1e-400,"rounded":9007199254740993.0,"integer":9007199254740993}`))
	if err != nil || string(got) != `{"gain":0.0,"integer":9007199254740993,"rounded":9007199254740992.0}` {
		t.Fatalf("wrong dispatched params: %s %v", got, err)
	}
}

// TestClosedInputSchemaGate pins the citadel-cli#1161 gate: an approved
// input_schema must be closed (additionalProperties:false on every object
// subschema, no case-fold-colliding property keys, no construct the walk cannot
// reason about), so an alternate-cased extra param key cannot slip past the schema
// and then bind a constrained field case-insensitively inside a builtin.
func TestClosedInputSchemaGate(t *testing.T) {
	const closed = `{"type":"object","additionalProperties":false,"properties":{"fps":{"type":"integer","maximum":60}}}`

	refused := []struct {
		name, schema, params, want string
	}{
		{
			// (1) root not closed is refused, even with otherwise-valid params.
			"open_root",
			`{"type":"object","properties":{"fps":{"type":"integer"}}}`,
			`{"fps":30}`,
			"additionalProperties",
		},
		{
			// (2) recursion: a nested object left open re-opens the hole one level down.
			"open_nested_object",
			`{"type":"object","additionalProperties":false,"properties":{"video":{"type":"object","properties":{"fps":{"type":"integer"}}}}}`,
			`{"video":{"fps":30}}`,
			"additionalProperties",
		},
		{
			// (3) an alternate-cased extra key is rejected by the compiled schema
			// itself once additionalProperties:false is enforced.
			"alt_cased_key_rejected_by_schema",
			closed,
			`{"Fps":9999}`,
			"violate input_schema",
		},
		{
			// (4) two properties colliding under case-fold is a bind ambiguity.
			"case_fold_property_collision",
			`{"type":"object","additionalProperties":false,"properties":{"fps":{"type":"integer"},"Fps":{"type":"integer"}}}`,
			`{}`,
			"collide under case-fold",
		},
		{
			// (5a) an external / non-local $ref is forbidden.
			"external_ref",
			`{"type":"object","$ref":"https://evil.example/schema.json"}`,
			`{}`,
			"forbidden",
		},
		{
			// (5b) patternProperties cannot be reasoned about; fail closed.
			"pattern_properties",
			`{"type":"object","additionalProperties":false,"patternProperties":{"^x":{"type":"string"}}}`,
			`{}`,
			"patternProperties",
		},
		{
			// (5c) a schema-valued additionalProperties is not "exactly false".
			"schema_valued_additional_properties",
			`{"type":"object","additionalProperties":{"type":"string"}}`,
			`{}`,
			"exactly false",
		},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTemplateParams(json.RawMessage(tc.params), json.RawMessage(tc.schema), json.RawMessage(`{}`))
			if err == nil {
				t.Fatalf("closed-schema gate accepted %s / %s", tc.schema, tc.params)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}

	// A well-formed closed schema with an exactly-declared key still validates.
	if err := ValidateTemplateParams(json.RawMessage(`{"fps":30}`), json.RawMessage(closed), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("closed schema rejected valid params: %v", err)
	}
	// output_schema is deliberately NOT subjected to the gate (it constrains what a
	// builtin produces, not caller-controlled params): an open output_schema is fine.
	if err := ValidateTemplateParams(json.RawMessage(`{"fps":30}`), json.RawMessage(closed), json.RawMessage(`{"type":"object","properties":{"mix":{"type":"string"}}}`)); err != nil {
		t.Fatalf("open output_schema should be allowed: %v", err)
	}
}

// TestClosedInputSchemaGateClosesRealBindHole proves both halves the gate exists
// for: Go's encoding/json really does bind an alternate-cased key to a constrained
// struct field (the hole), and ValidateTemplateParams refuses that same payload
// against a closed schema before any builtin could decode it (the fix).
func TestClosedInputSchemaGateClosesRealBindHole(t *testing.T) {
	// The hole: a builtin decoding params into its own struct binds "Fps" to the
	// field tagged `json:"fps"` case-insensitively, bypassing a "fps" constraint.
	type mixParams struct {
		Fps int `json:"fps"`
	}
	var mp mixParams
	if err := json.Unmarshal([]byte(`{"Fps":9999}`), &mp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if mp.Fps != 9999 {
		t.Fatal("expected encoding/json to bind the alternate-cased key (the hole this gate closes)")
	}

	// The fix: the same payload is refused at validation against a closed schema,
	// so it never reaches a builtin decode.
	const closed = `{"type":"object","additionalProperties":false,"properties":{"fps":{"type":"integer","maximum":60}}}`
	err := ValidateTemplateParams(json.RawMessage(`{"Fps":9999}`), json.RawMessage(closed), json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "violate input_schema") {
		t.Fatalf("gate did not refuse the alt-cased key: %v", err)
	}
}
