package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// DecodeTemplateJSON has one interpretation: duplicate keys, trailing JSON,
// and invalid UTF-8 are refused rather than silently selecting another value.
func DecodeTemplateJSON(raw json.RawMessage) (any, error) {
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("invalid JSON UTF-8")
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("invalid JSON")
	}
	// encoding/json replaces lone UTF-16 surrogates with U+FFFD, whereas Python
	// retains them. Refuse that unsupported wire representation rather than hash
	// or dispatch a lossy interpretation. Escaped backslashes are skipped.
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		if raw[i+1] != 'u' {
			i++
			continue
		}
		code, _ := strconv.ParseUint(string(raw[i+2:i+6]), 16, 16)
		if code >= 0xd800 && code <= 0xdbff {
			if i+12 > len(raw) || string(raw[i+6:i+8]) != `\u` {
				return nil, fmt.Errorf("unpaired JSON surrogate")
			}
			low, err := strconv.ParseUint(string(raw[i+8:i+12]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return nil, fmt.Errorf("unpaired JSON surrogate")
			}
			i += 11
		} else if code >= 0xdc00 && code <= 0xdfff {
			return nil, fmt.Errorf("unpaired JSON surrogate")
		} else {
			i += 5
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, err := decodeTemplateValue(d)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON: %v", err)
	}
	return v, nil
}

func decodeTemplateValue(d *json.Decoder) (any, error) {
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		m := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			k, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("object key must be a string")
			}
			if _, exists := m[k]; exists {
				return nil, fmt.Errorf("duplicate JSON key %q", k)
			}
			v, err := decodeTemplateValue(d)
			if err != nil {
				return nil, err
			}
			m[k] = v
		}
		if _, err := d.Token(); err != nil {
			return nil, err
		}
		return m, nil
	case '[':
		a := []any{}
		for d.More() {
			v, err := decodeTemplateValue(d)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		if _, err := d.Token(); err != nil {
			return nil, err
		}
		return a, nil
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

type TemplateRunner struct{ Kind, Handler string }

// ParseTemplateRunner reads exact keys from the same decoder used for hashing.
// The runner vocabulary is closed: no case aliases or extra executable fields.
func ParseTemplateRunner(raw json.RawMessage) (TemplateRunner, error) {
	var r TemplateRunner
	v, err := DecodeTemplateJSON(raw)
	if err != nil {
		return r, fmt.Errorf("decode runner descriptor: %w", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return r, fmt.Errorf("runner must be an object")
	}
	for k := range m {
		if k != "kind" && k != "handler" {
			return r, fmt.Errorf("noncanonical runner key %q", k)
		}
	}
	r.Kind, ok = m["kind"].(string)
	if !ok || r.Kind != strings.TrimSpace(r.Kind) {
		return r, fmt.Errorf("runner kind must be an exact string")
	}
	r.Handler, ok = m["handler"].(string)
	if !ok || r.Handler != strings.TrimSpace(r.Handler) {
		return r, fmt.Errorf("runner handler must be an exact string")
	}
	return r, nil
}

// ValidateTemplateParams compiles the approved schema without fetching files or
// URLs. References within the supplied schema work; external resources fail closed.
func ValidateTemplateParams(params, inputSchema, outputSchema json.RawMessage) error {
	normalized, err := NormalizeTemplateParams(params)
	if err != nil {
		return err
	}
	v, err := DecodeTemplateJSON(normalized)
	if err != nil {
		return err
	}
	for _, field := range []struct {
		name string
		raw  json.RawMessage
	}{{"input_schema", inputSchema}, {"output_schema", outputSchema}} {
		schemaValue, err := DecodeTemplateJSON(field.raw)
		if err != nil {
			return fmt.Errorf("%s: %w", field.name, err)
		}
		schemaMap, ok := schemaValue.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be an object", field.name)
		}
		// citadel-cli#1161 gate: an approved input_schema must be "closed" so a
		// param key that is not an exactly-declared property is rejected here,
		// before any builtin decodes params into a Go struct (where encoding/json
		// would bind an alternate-cased key case-insensitively and bypass the
		// schema). Applies to input_schema only: params are the caller-controlled
		// surface; output_schema constrains what a builtin produces.
		if field.name == "input_schema" {
			if err := enforceClosedInputSchema(schemaMap); err != nil {
				return fmt.Errorf("input_schema: %w", err)
			}
		}
		c := jsonschema.NewCompiler()
		c.LoadURL = func(url string) (io.ReadCloser, error) {
			return nil, fmt.Errorf("external schema resource %q is forbidden", url)
		}
		const base = "https://citadel.invalid/template-schema.json"
		// Compile precisely the Python-normalized schema committed by the hash.
		// A raw underflow/rounded float must not impose different limits under
		// the same approved hash when the validator supports exact rationals.
		var canonical bytes.Buffer
		if err := writeCanonicalJSON(&canonical, schemaValue); err != nil {
			return fmt.Errorf("%s: %w", field.name, err)
		}
		if err := c.AddResource(base, bytes.NewReader(canonical.Bytes())); err != nil {
			return fmt.Errorf("%s: %w", field.name, err)
		}
		schema, err := c.Compile(base)
		if err != nil {
			return fmt.Errorf("%s: %w", field.name, err)
		}
		if field.name == "input_schema" {
			if err := schema.Validate(v); err != nil {
				return fmt.Errorf("params violate input_schema: %w", err)
			}
		}
	}
	return nil
}

// NormalizeTemplateParams ensures validation and builtin dispatch see the same
// Python-compatible numeric values, even after a legal wire reserialization.
func NormalizeTemplateParams(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	v, err := DecodeTemplateJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("params: %w", err)
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, fmt.Errorf("params must be an object")
	}
	var b bytes.Buffer
	if err := writeCanonicalJSON(&b, v); err != nil {
		return nil, fmt.Errorf("params: %w", err)
	}
	return b.Bytes(), nil
}

type TemplateInputFile struct{ Path, NodeID, NodePath string }

// ParseTemplateInputFiles validates every reference before any filesystem work.
// NodeID is supplied by local worker configuration, never by the job payload.
func ParseTemplateInputFiles(raw json.RawMessage, nodeID string) ([]TemplateInputFile, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	v, err := DecodeTemplateJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("input_files: %w", err)
	}
	a, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("input_files must be an array")
	}
	files := make([]TemplateInputFile, 0, len(a))
	for _, entry := range a {
		m, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("input_files entry must be an object")
		}
		for k := range m {
			if k != "path" && k != "node_id" && k != "node_path" {
				return nil, fmt.Errorf("noncanonical input_files key %q", k)
			}
		}
		f := TemplateInputFile{}
		f.Path, _ = m["path"].(string)
		f.NodeID, _ = m["node_id"].(string)
		f.NodePath, _ = m["node_path"].(string)
		if nodeID == "" || f.NodeID == "" || f.NodeID != nodeID {
			return nil, fmt.Errorf("input_files node_id %q does not match executing node %q", f.NodeID, nodeID)
		}
		if f.NodePath == "" || f.NodePath != strings.TrimSpace(f.NodePath) || path.IsAbs(f.NodePath) || path.Clean(f.NodePath) != f.NodePath || strings.ContainsAny(f.NodePath, "\\\x00:") || f.NodePath == ".." || f.NodePath == "." || strings.HasPrefix(f.NodePath, "../") {
			return nil, fmt.Errorf("input_files node_path must be a canonical workspace-relative path")
		}
		if f.Path != f.NodePath && f.Path != "node:"+f.NodeID+"/"+f.NodePath {
			return nil, fmt.Errorf("input_files path %q disagrees with node_id/node_path", f.Path)
		}
		files = append(files, f)
	}
	return files, nil
}

// enforceClosedInputSchema requires an approved template input_schema to be
// "closed": every object subschema must declare "additionalProperties": false, so
// a params key that is not an exactly-declared property is rejected at validation.
// This is the citadel-cli#1161 gate. Without it, Go's encoding/json binds struct
// fields case-insensitively, so an alternate-cased extra key (e.g. "Fps" against a
// field tagged `json:"fps"`) passes the schema as an unconstrained additional
// property and then binds the constrained field inside a builtin, bypassing the
// schema. It fails closed on any construct it cannot reason about ($ref,
// patternProperties, a schema-valued additionalProperties, unevaluatedProperties)
// and on properties keys that collide under case-fold (another bind ambiguity).
// Nothing is approved yet, so this strictness costs no compatibility; loosening it
// later is a one-line change, whereas tightening it later is a hash-breaking
// re-approval of every template.
func enforceClosedInputSchema(root map[string]any) error {
	if !schemaDescribesObject(root) {
		return fmt.Errorf("root must be an object schema (\"type\":\"object\" with \"additionalProperties\": false)")
	}
	return walkClosedSchema(root, "")
}

// walkClosedSchema enforces the closed-object rule on node and recurses into every
// place a subschema can legally appear.
func walkClosedSchema(node any, at string) error {
	m, ok := node.(map[string]any)
	if !ok {
		// A boolean or scalar subschema node constrains nothing a builtin binds.
		return nil
	}
	if ref, present := m["$ref"]; present {
		// A local "#/..." ref is allowed: its target lives in this same schema
		// ($defs/definitions/properties), which walkClosedSchema also visits and
		// requires closed, so the referenced object subschema is enforced too. An
		// external or non-local ref is forbidden (also blocked at compile by the
		// LoadURL hook, but rejected here first so the closed-schema reasoning holds).
		s, ok := ref.(string)
		if !ok || !strings.HasPrefix(s, "#") {
			return fmt.Errorf("external or non-local $ref at %s is forbidden; approved schemas must be self-contained", schemaPathOrRoot(at))
		}
	}
	for _, forbidden := range []string{"$dynamicRef", "patternProperties", "unevaluatedProperties"} {
		if _, present := m[forbidden]; present {
			return fmt.Errorf("unsupported schema construct %q at %s (approved schemas must be closed and self-contained)", forbidden, schemaPathOrRoot(at))
		}
	}
	if schemaDescribesObject(m) {
		ap, present := m["additionalProperties"]
		if !present {
			return fmt.Errorf("object schema at %s must declare \"additionalProperties\": false", schemaPathOrRoot(at))
		}
		if b, ok := ap.(bool); !ok || b {
			return fmt.Errorf("\"additionalProperties\" at %s must be exactly false, not true or a schema", schemaPathOrRoot(at))
		}
		if props, ok := m["properties"].(map[string]any); ok {
			seen := map[string]string{}
			for k := range props {
				lk := strings.ToLower(k)
				if prev, dup := seen[lk]; dup {
					return fmt.Errorf("properties keys %q and %q at %s collide under case-fold", prev, k, schemaPathOrRoot(at))
				}
				seen[lk] = k
			}
			for k, sub := range props {
				if err := walkClosedSchema(sub, at+".properties."+k); err != nil {
					return err
				}
			}
		}
	}
	switch items := m["items"].(type) {
	case map[string]any:
		if err := walkClosedSchema(items, at+".items"); err != nil {
			return err
		}
	case []any:
		for i, sub := range items {
			if err := walkClosedSchema(sub, fmt.Sprintf("%s.items[%d]", at, i)); err != nil {
				return err
			}
		}
	}
	for _, comb := range []string{"allOf", "anyOf", "oneOf"} {
		if arr, ok := m[comb].([]any); ok {
			for i, sub := range arr {
				if err := walkClosedSchema(sub, fmt.Sprintf("%s.%s[%d]", at, comb, i)); err != nil {
					return err
				}
			}
		}
	}
	for _, defs := range []string{"$defs", "definitions"} {
		if dm, ok := m[defs].(map[string]any); ok {
			for k, sub := range dm {
				if err := walkClosedSchema(sub, at+"."+defs+"."+k); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// schemaDescribesObject reports whether a schema node constrains an object: it
// declares "properties" or its "type" is (or includes) "object".
func schemaDescribesObject(m map[string]any) bool {
	if _, ok := m["properties"]; ok {
		return true
	}
	switch t := m["type"].(type) {
	case string:
		return t == "object"
	case []any:
		for _, v := range t {
			if s, ok := v.(string); ok && s == "object" {
				return true
			}
		}
	}
	return false
}

func schemaPathOrRoot(at string) string {
	if at == "" {
		return "(root)"
	}
	return strings.TrimPrefix(at, ".")
}
