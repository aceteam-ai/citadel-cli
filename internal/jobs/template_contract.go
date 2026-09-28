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
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	v, err := DecodeTemplateJSON(params)
	if err != nil {
		return fmt.Errorf("params: %w", err)
	}
	if _, ok := v.(map[string]any); !ok {
		return fmt.Errorf("params must be an object")
	}
	for _, field := range []struct {
		name string
		raw  json.RawMessage
	}{{"input_schema", inputSchema}, {"output_schema", outputSchema}} {
		schemaValue, err := DecodeTemplateJSON(field.raw)
		if err != nil {
			return fmt.Errorf("%s: %w", field.name, err)
		}
		if _, ok := schemaValue.(map[string]any); !ok {
			return fmt.Errorf("%s must be an object", field.name)
		}
		c := jsonschema.NewCompiler()
		c.LoadURL = func(url string) (io.ReadCloser, error) {
			return nil, fmt.Errorf("external schema resource %q is forbidden", url)
		}
		const base = "https://citadel.invalid/template-schema.json"
		if err := c.AddResource(base, bytes.NewReader(field.raw)); err != nil {
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
