package workercontrol

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Decode rejects ambiguity before struct decoding loses duplicate names,
// lexical integer forms or malformed Unicode. Every schema field is required.
func Decode(raw []byte, out any, limit int) error {
	if len(raw) > limit || !utf8.Valid(raw) || !validEscapes(raw) {
		return ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	value, e := readValue(d, 0, false)
	if e != nil {
		return ErrInvalid
	}
	if _, e = d.Token(); e != io.EOF {
		return ErrInvalid
	}
	t := reflect.TypeOf(out)
	if t == nil || t.Kind() != reflect.Pointer || !shape(value, t.Elem()) {
		return ErrInvalid
	}
	if json.Unmarshal(raw, out) != nil {
		return ErrInvalid
	}
	return nil
}

// CheckJSON checks lexical ambiguity without imposing a generic transport schema.
// Case-folded duplicates are also rejected before generic struct decoding can
// overwrite a control-bearing field through an encoding/json alias.
func CheckJSON(raw []byte, limit int) error {
	if len(raw) > limit || !utf8.Valid(raw) || !validEscapes(raw) {
		return ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if _, e := readValue(d, 0, true); e != nil {
		return ErrInvalid
	}
	if _, e := d.Token(); e != io.EOF {
		return ErrInvalid
	}
	return nil
}

// HasStringField classifies only the named JSON path, preserving every duplicate
// occurrence. Keys follow encoding/json's case folding so aliases cannot evade
// the strict decoder. A "*" path component visits array elements, never objects.
// This is classification only; callers must still validate the complete schema.
func HasStringField(raw []byte, want string, path ...string) bool {
	return hasField(raw, path, func(t json.Token) bool { return t == want })
}

func hasField(raw []byte, path []string, match func(json.Token) bool) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	found, _ := scanField(d, path, match)
	return found
}

func scanField(d *json.Decoder, path []string, match func(json.Token) bool) (bool, error) {
	if len(path) == 0 && match == nil {
		return true, nil // Field presence also classifies malformed control data.
	}
	t, err := d.Token()
	if err != nil {
		return false, err
	}
	if len(path) == 0 {
		if match(t) {
			return true, nil
		}
		// Consume a non-string value without interpreting its descendants.
		if delim, ok := t.(json.Delim); ok && (delim == '{' || delim == '[') {
			depth := 1
			for depth > 0 {
				t, err = d.Token()
				if err != nil {
					return false, err
				}
				if delim, ok := t.(json.Delim); ok {
					switch delim {
					case '{', '[':
						depth++
					case '}', ']':
						depth--
					}
				}
			}
		}
		return false, nil
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return false, nil
	}
	for d.More() {
		selected := delim == '[' && path[0] == "*"
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return false, err
			}
			name, ok := key.(string)
			selected = ok && path[0] != "*" && strings.EqualFold(name, path[0])
		}
		if selected {
			found, err := scanField(d, path[1:], match)
			if found || err != nil {
				return found, err
			}
		} else {
			var discard json.RawMessage
			if err := d.Decode(&discard); err != nil {
				return false, err
			}
		}
	}
	_, err = d.Token()
	return false, err
}
func readValue(d *json.Decoder, depth int, foldKeys bool) (any, error) {
	if depth > 8 {
		return nil, ErrInvalid
	}
	t, e := d.Token()
	if e != nil {
		return nil, e
	}
	switch v := t.(type) {
	case json.Delim:
		if v == '{' {
			m := map[string]any{}
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return nil, e
				}
				s, ok := k.(string)
				if !ok {
					return nil, ErrInvalid
				}
				key := s
				if foldKeys {
					key = strings.Map(func(r rune) rune {
						lowest := r
						for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
							if next < lowest {
								lowest = next
							}
						}
						return lowest
					}, key)
				}
				if seen[key] {
					return nil, ErrInvalid
				}
				seen[key] = true
				x, e := readValue(d, depth+1, foldKeys)
				if e != nil {
					return nil, e
				}
				m[s] = x
			}
			if _, e := d.Token(); e != nil {
				return nil, e
			}
			return m, nil
		}
		if v == '[' {
			a := []any{}
			for d.More() {
				x, e := readValue(d, depth+1, foldKeys)
				if e != nil {
					return nil, e
				}
				a = append(a, x)
			}
			if _, e := d.Token(); e != nil {
				return nil, e
			}
			return a, nil
		}
		return nil, ErrInvalid
	case json.Number:
		if !positiveText.MatchString(string(v)) {
			return nil, ErrInvalid
		}
		n, e := strconv.ParseInt(string(v), 10, 64)
		if e != nil || !integer(n) {
			return nil, ErrInvalid
		}
		return v, nil
	default:
		return t, nil
	}
}
func shape(v any, t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		return v == nil || shape(v, t.Elem())
	}
	if v == nil {
		return false
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok {
			return false
		}
		count := 0
		for n := 0; n < t.NumField(); n++ {
			f := t.Field(n)
			key := strings.Split(f.Tag.Get("json"), ",")[0]
			if key == "-" || f.PkgPath != "" {
				continue
			}
			if key == "" {
				key = f.Name
			}
			count++
			x, ok := m[key]
			if !ok || !shape(x, f.Type) {
				return false
			}
		}
		return len(m) == count
	case reflect.String:
		_, ok := v.(string)
		return ok
	case reflect.Int64:
		_, ok := v.(json.Number)
		return ok
	case reflect.Bool:
		_, ok := v.(bool)
		return ok
	case reflect.Slice:
		a, ok := v.([]any)
		if !ok {
			return false
		}
		for _, x := range a {
			if !shape(x, t.Elem()) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// Check every JSON string escape, including surrogate pairing. encoding/json
// otherwise replaces lone surrogates with U+FFFD before validation.
func validEscapes(raw []byte) bool {
	for n := 0; n < len(raw); n++ {
		if raw[n] != '"' {
			continue
		}
		n++
		for ; n < len(raw) && raw[n] != '"'; n++ {
			if raw[n] != '\\' {
				continue
			}
			n++
			if n >= len(raw) {
				return false
			}
			if raw[n] != 'u' {
				continue
			}
			if n+4 >= len(raw) {
				return false
			}
			v, e := strconv.ParseUint(string(raw[n+1:n+5]), 16, 16)
			if e != nil {
				return false
			}
			n += 4
			if v >= 0xdc00 && v <= 0xdfff {
				return false
			}
			if v >= 0xd800 && v <= 0xdbff {
				if n+6 >= len(raw) || raw[n+1] != '\\' || raw[n+2] != 'u' {
					return false
				}
				w, e := strconv.ParseUint(string(raw[n+3:n+7]), 16, 16)
				if e != nil || w < 0xdc00 || w > 0xdfff {
					return false
				}
				n += 6
			}
		}
		if n >= len(raw) {
			return false
		}
	}
	return true
}

// LooksControl classifies transport and stream-job discriminants, including
// overwritten duplicates. Application content is never a routing discriminant.
func LooksControl(raw []byte) bool {
	return hasField(raw, []string{"type"}, func(t json.Token) bool {
		s, _ := t.(string)
		return s == "worker_control" || s == "WORKER_CONTROL" || s == "worker_control_response" || s == "worker_control_request"
	}) || hasField(raw, []string{"queue"}, func(t json.Token) bool {
		s, _ := t.(string)
		return IsQueue(s)
	}) || LooksControlData(raw, "data")
}

// LooksControlData checks only the stream data object at the supplied path.
func LooksControlData(raw []byte, path ...string) bool {
	typePath := append(append([]string{}, path...), "type")
	controlPath := append(append([]string{}, path...), "worker_control")
	return HasStringField(raw, "WORKER_CONTROL", typePath...) || hasField(raw, controlPath, nil)
}

type Data struct {
	JobID         string `json:"jobId"`
	Type          string `json:"type"`
	Payload       string `json:"payload"`
	WorkerControl string `json:"worker_control"`
}

func ParseData(raw []byte, id, queue, group string) (Envelope, Payload, error) {
	var d Data
	var e Envelope
	var p Payload
	if Decode(raw, &d, 20000) != nil || d.Type != "WORKER_CONTROL" || len(d.WorkerControl) > 8192 || len(d.Payload) > 256 || Decode([]byte(d.WorkerControl), &e, 8192) != nil || Decode([]byte(d.Payload), &p, 256) != nil || e.Identity.Validate() != nil || e.Delivery.Validate(e.Identity) != nil || d.JobID != e.Identity.JobID || id != e.Delivery.MessageID || (queue != "" && queue != e.Identity.Queue) || (group != "" && group != Protocol) || p.Action != "restart" || p.TargetNode != e.Identity.TargetNode || PayloadDigest(p) != e.Identity.PayloadDigest {
		return Envelope{}, Payload{}, ErrInvalid
	}
	return e, p, nil
}
