package jobprivacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

var ErrMediaProjection = errors.New("private media execution projection unavailable")

// MediaPayload owns existing execution fields and exact original bytes. It is
// NOT a permission, authenticated delivery identity or execution approval. Only
// trusted intake/adapters should explicitly project it; diagnostics must not.
// Passive deliverymetadata.Observation remains separate and unchanged.
type MediaPayload struct {
	snapshot func() mediaRecord
}

type mediaRecord struct {
	payload map[string]any
	raw     []byte
	data    map[string]any
	invalid bool
}

// CaptureMedia returns nil for ordinary jobs. A non-nil result always represents
// private data, even when copying an unsupported/deep value failed. Callers must
// clear their observable payload/raw fields in that case too. Invalid captures
// retain exact raw byte identity but refuse execution/data projection.
func CaptureMedia(kind string, payload map[string]any, raw []byte, data map[string]any) *MediaPayload {
	if !privateMediaFields(kind, payload, raw, data) {
		return nil
	}
	budget := 4096
	owned, err := ownMediaValue(payload, 0, &budget)
	ownedData, dataErr := ownMediaValue(data, 0, &budget)
	record := mediaRecord{raw: bytes.Clone(raw), invalid: err != nil || dataErr != nil}
	if !record.invalid {
		record.payload, _ = owned.(map[string]any)
		record.data, _ = ownedData.(map[string]any)
	}
	return &MediaPayload{snapshot: func() mediaRecord { return record }}
}

// PrivateMediaMarker has no execution fields. Its presence preserves private
// classification after intake has removed raw credential-bearing values.
func PrivateMediaMarker() map[string]any {
	return map[string]any{"inputs": "[REDACTED]"}
}

func (p *MediaPayload) record() mediaRecord {
	if p == nil || p.snapshot == nil {
		return mediaRecord{invalid: true}
	}
	return p.snapshot()
}

func (p *MediaPayload) Clone() *MediaPayload {
	if p == nil {
		return nil
	}
	copy := *p
	return &copy
}

// ExecutionPayload returns an independent copy with original Go value types.
// It never parses raw-only observations into executable fields or reserializes
// JSON. A raw-only/nil/invalid capture grants no execution projection.
func (p *MediaPayload) ExecutionPayload() (map[string]any, error) {
	record := p.record()
	if record.invalid || record.payload == nil {
		return nil, ErrMediaProjection
	}
	budget := 4096
	copy, err := ownMediaValue(record.payload, 0, &budget)
	if err != nil {
		return nil, ErrMediaProjection
	}
	return copy.(map[string]any), nil
}

// ExecutionField projects one original field for trusted routing/accounting
// consumers, without copying unrelated credential-bearing fields.
func (p *MediaPayload) ExecutionField(key string) (any, bool) {
	record := p.record()
	if record.invalid || record.payload == nil {
		return nil, false
	}
	value, present := record.payload[key]
	if !present {
		return nil, false
	}
	budget := 4096
	copy, err := ownMediaValue(value, 0, &budget)
	return copy, err == nil
}

// OriginalPayload returns the exact provided bytes, preserving nil/empty and
// every number/Unicode/whitespace lexeme. No decoded-map fallback is invented.
func (p *MediaPayload) OriginalPayload() []byte {
	return bytes.Clone(p.record().raw)
}

// OriginalData is an explicit trusted source-conversion projection, not a
// diagnostic representation or authority derived from passive observations.
func (p *MediaPayload) OriginalData() (map[string]any, error) {
	record := p.record()
	if record.invalid {
		return nil, ErrMediaProjection
	}
	budget := 4096
	copy, err := ownMediaValue(record.data, 0, &budget)
	if err != nil {
		return nil, ErrMediaProjection
	}
	data, _ := copy.(map[string]any)
	return data, nil
}

func (MediaPayload) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "<private media payload>")
}

func (MediaPayload) MarshalJSON() ([]byte, error) {
	return []byte(`"[REDACTED]"`), nil
}

// Copy only known immutable scalars and owned containers, never call arbitrary
// MarshalJSON/String methods. The depth/value budgets also terminate cycles.
// Refusing an unsupported representation does not make it executable.
func ownMediaValue(value any, depth int, budget *int) (any, error) {
	*budget -= 1
	if depth > 32 || *budget < 0 {
		return nil, ErrMediaProjection
	}
	switch value := value.(type) {
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return value, nil
	case string:
		return strings.Clone(value), nil
	case json.Number:
		return json.Number(strings.Clone(string(value))), nil
	case []byte:
		return bytes.Clone(value), nil
	case json.RawMessage:
		return json.RawMessage(bytes.Clone(value)), nil
	case map[string]any:
		if value == nil {
			return map[string]any(nil), nil
		}
		if len(value) > *budget {
			return nil, ErrMediaProjection
		}
		copy := make(map[string]any, len(value))
		for key, item := range value {
			owned, err := ownMediaValue(item, depth+1, budget)
			if err != nil {
				return nil, err
			}
			copy[strings.Clone(key)] = owned
		}
		return copy, nil
	case []any:
		if value == nil {
			return []any(nil), nil
		}
		if len(value) > *budget {
			return nil, ErrMediaProjection
		}
		copy := make([]any, len(value))
		for i, item := range value {
			owned, err := ownMediaValue(item, depth+1, budget)
			if err != nil {
				return nil, err
			}
			copy[i] = owned
		}
		return copy, nil
	case []string:
		if value == nil {
			return []string(nil), nil
		}
		if len(value) > *budget {
			return nil, ErrMediaProjection
		}
		copy := make([]string, len(value))
		for i, item := range value {
			owned, err := ownMediaValue(item, depth+1, budget)
			if err != nil {
				return nil, err
			}
			copy[i] = owned.(string)
		}
		return copy, nil
	case map[string]string:
		if value == nil {
			return map[string]string(nil), nil
		}
		if len(value) > *budget {
			return nil, ErrMediaProjection
		}
		copy := make(map[string]string, len(value))
		for key, item := range value {
			owned, err := ownMediaValue(item, depth+1, budget)
			if err != nil {
				return nil, err
			}
			copy[strings.Clone(key)] = owned.(string)
		}
		return copy, nil
	default:
		return nil, ErrMediaProjection
	}
}

func privateMediaFields(kind string, payload map[string]any, raw []byte, data map[string]any) bool {
	if kind != MediaGenerate {
		return false
	}
	if IsPrivateMedia(kind, payload) || privateMediaRaw(raw) {
		return true
	}
	if value, present := data["payload"]; present {
		switch value := value.(type) {
		case nil:
			return false
		case string:
			return privateMediaRaw([]byte(value))
		case []byte:
			return privateMediaRaw(value)
		default:
			return true
		}
	}
	return false
}

// Inspect root keys without decoding/allocating large prompt values. Invalid
// nonempty media payloads are private. Valid legacy objects without inputs keep
// their existing retention, regardless of prompt size. This is classification
// only: it cannot create executable fields from a raw-only observation.
func privateMediaRaw(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	if !json.Valid(raw) {
		return true
	}
	space := func(i int) int {
		for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\r' || raw[i] == '\n') {
			i++
		}
		return i
	}
	i := space(0)
	if raw[i] != '{' {
		return true
	}
	i = space(i + 1)
	for raw[i] != '}' {
		start := i
		i++
		for raw[i] != '"' {
			if raw[i] == '\\' {
				i++
			}
			i++
		}
		i++
		// Six ASCII characters have at most 36 escaped bytes plus quotes.
		if i-start <= 38 {
			var name string
			if json.Unmarshal(raw[start:i], &name) == nil && name == "inputs" {
				return true
			}
		}
		i = space(i)
		i = space(i + 1) // json.Valid established the colon and full value.
		depth, quoted, escaped := 0, false, false
		for i < len(raw) {
			b := raw[i]
			if quoted {
				if escaped {
					escaped = false
				} else if b == '\\' {
					escaped = true
				} else if b == '"' {
					quoted = false
				}
			} else {
				if depth == 0 && (b == ',' || b == '}') {
					break
				}
				switch b {
				case '"':
					quoted = true
				case '{', '[':
					depth++
				case '}', ']':
					depth--
				}
			}
			i++
		}
		if raw[i] == '}' {
			break
		}
		i = space(i + 1)
	}
	return false
}
