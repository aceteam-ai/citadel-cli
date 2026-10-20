// Package mediainput defines the inactive media_input.v1 input contract. Parsing
// an input descriptor grants no file, capability, model or execution authority.
package mediainput

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	Schema           = "media_input.v1"
	TaskImageToImage = "image-to-image"
	TaskInpaint      = "inpaint"
	MaxFileBytes     = 16 << 20
	MaxEnvelopeBytes = 10 << 10
	MaxTokenBytes    = 4096
)

// Descriptor identifies the exact bytes to acquire later. It is not a verified
// snapshot and must not be passed to a model without acquisition and validation.
type Descriptor struct {
	Role       string
	Origin     string
	SHA256     string
	Size       int64
	Format     string
	Path       string
	Capability Capability
}

// Capability prevents accidental credential disclosure in diagnostics or JSON.
// Reveal is the explicit escape hatch for the pinned, authorized fetch client.
// The bytes live only in a closure: fmt bypasses methods while traversing an
// unexported wrapper field, but cannot traverse a function's captured values.
type Capability struct{ reveal func() string }

func (c Capability) Reveal() string {
	if c.reveal == nil {
		return ""
	}
	return c.reveal()
}

func (c Capability) String() string {
	return "[REDACTED]"
}

func (c Capability) GoString() string {
	return c.String()
}

func (c Capability) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, c.String())
}

func (c Capability) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED]"`), nil
}

func (c Capability) LogValue() slog.Value {
	return slog.StringValue(c.String())
}

// Contract owns its parsed descriptor list. Accessors return copies, so a caller
// cannot mutate the validated contract. Treat inbox capabilities as credentials.
type Contract struct {
	inputs []Descriptor
}

func (c Contract) Inputs() []Descriptor {
	return append([]Descriptor(nil), c.inputs...)
}

// Format also protects the unexported descriptor slice: fmt does not invoke
// nested formatting methods when traversing unexported struct fields.
func (c Contract) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "<private media-input contract>")
}

// Refusal contains only a static reason, never input paths or capabilities.
type Refusal struct{ reason string }

func (e *Refusal) Error() string {
	return fmt.Sprintf(`{"reason":%q,"message":"MEDIA_INPUT refused"}`, e.reason)
}

func (e *Refusal) Reason() string { return e.reason }

func refuse(reason string) error { return &Refusal{reason: reason} }

// Parse accepts exact, case-sensitive keysets and source-first ordering. Unknown
// keys, duplicate or aliased keys, coercions and malformed Unicode are refused.
// It performs no I/O and does not validate an opaque capability's signature.
func Parse(task string, raw []byte) (Contract, error) {
	invalid := refuse("invalid_schema")
	if task != TaskImageToImage && task != TaskInpaint {
		return Contract{}, refuse("unsupported_task")
	}
	if len(raw) == 0 || len(raw) > MaxEnvelopeBytes || !utf8.Valid(raw) || !validUnicodeEscapes(raw) {
		return Contract{}, invalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	budget := 128
	value, err := readValue(d, 0, &budget)
	if err != nil {
		return Contract{}, invalid
	}
	if _, err := d.Token(); err != io.EOF {
		return Contract{}, invalid
	}
	root, ok := value.(map[string]any)
	if !ok || !exactKeys(root, "schema", "inputs") || root["schema"] != Schema {
		return Contract{}, invalid
	}
	items, ok := root["inputs"].([]any)
	want := 1
	if task == TaskInpaint {
		want = 2
	}
	if !ok || len(items) != want {
		return Contract{}, refuse("invalid_roles")
	}
	inputs := make([]Descriptor, 0, want)
	for i, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return Contract{}, invalid
		}
		location := "path"
		if m["origin"] == "inbox" {
			location = "capability"
		}
		if !exactKeys(m, "role", "origin", "sha256", "size", "format", location) {
			return Contract{}, invalid
		}
		for _, key := range []string{"role", "origin", "sha256", "format", location} {
			if _, ok := m[key].(string); !ok {
				return Contract{}, invalid
			}
		}
		role := "source"
		if i == 1 {
			role = "mask"
		}
		if m["role"] != role {
			return Contract{}, refuse("invalid_roles")
		}
		n, ok := m["size"].(json.Number)
		if !ok {
			return Contract{}, invalid
		}
		size, err := strconv.ParseInt(string(n), 10, 64)
		if err != nil || size < 1 || size > MaxFileBytes {
			return Contract{}, refuse("invalid_size")
		}
		hash := m["sha256"].(string)
		if len(hash) != 64 || hash != strings.ToLower(hash) {
			return Contract{}, refuse("invalid_sha256")
		}
		if _, err := hex.DecodeString(hash); err != nil {
			return Contract{}, refuse("invalid_sha256")
		}
		format := m["format"].(string)
		if (format != "png" && format != "jpeg" && format != "webp") || (role == "mask" && format != "png") {
			return Contract{}, refuse("invalid_format")
		}
		input := Descriptor{Role: role, Origin: m["origin"].(string), SHA256: hash, Size: size, Format: format}
		switch input.Origin {
		case "workspace":
			input.Path = m["path"].(string)
			if !validPath(input.Path) {
				return Contract{}, refuse("invalid_input_path")
			}
		case "inbox":
			token := m["capability"].(string)
			input.Capability = Capability{reveal: func() string { return token }}
			if !validCapability(input.Capability.Reveal()) {
				return Contract{}, refuse("invalid_capability")
			}
		default:
			return Contract{}, refuse("invalid_origin")
		}
		inputs = append(inputs, input)
	}
	return Contract{inputs: inputs}, nil
}

func validPath(path string) bool {
	return path != "." && filepath.IsLocal(path) && !strings.ContainsAny(path, "\\:\x00") &&
		filepath.ToSlash(filepath.Clean(path)) == path && norm.NFC.IsNormalString(path) &&
		strings.IndexFunc(path, unicode.IsControl) < 0
}

func validCapability(token string) bool {
	if token == "" || token == "." || token == ".." || len(token) > MaxTokenBytes {
		return false
	}
	for _, c := range token {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func exactKeys(m map[string]any, keys ...string) bool {
	if len(m) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := m[key]; !ok {
			return false
		}
	}
	return true
}

// readValue is deliberately bounded before allocating maps/slices. Decoded keys
// are compared before insertion, so escaped duplicates cannot overwrite values.
func readValue(d *json.Decoder, depth int, budget *int) (any, error) {
	*budget -= 1
	if depth > 4 || *budget < 0 {
		return nil, refuse("invalid_schema")
	}
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
		m := make(map[string]any)
		for d.More() {
			key, err := d.Token()
			name, ok := key.(string)
			if err != nil || !ok || len(m) >= 8 {
				return nil, refuse("invalid_schema")
			}
			if _, duplicate := m[name]; duplicate {
				return nil, refuse("invalid_schema")
			}
			value, err := readValue(d, depth+1, budget)
			if err != nil {
				return nil, err
			}
			m[name] = value
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, refuse("invalid_schema")
		}
		return m, nil
	case '[':
		items := make([]any, 0, 2)
		for d.More() {
			if len(items) >= 2 {
				return nil, refuse("invalid_schema")
			}
			value, err := readValue(d, depth+1, budget)
			if err != nil {
				return nil, err
			}
			items = append(items, value)
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return nil, refuse("invalid_schema")
		}
		return items, nil
	default:
		return nil, refuse("invalid_schema")
	}
}

// encoding/json replaces lone UTF-16 surrogates. Refuse them first instead of
// accepting a different path or credential than the producer actually sent.
func validUnicodeEscapes(raw []byte) bool {
	inside := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' {
			inside = !inside
			continue
		}
		if !inside || raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		first, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if first >= 0xd800 && first <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			second, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if err != nil || second < 0xdc00 || second > 0xdfff {
				return false
			}
			i += 6
		} else if first >= 0xdc00 && first <= 0xdfff {
			return false
		}
	}
	return !inside
}
