package deliverymetadata

import (
	"encoding/json"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

var names = [5]string{"protocol", "dispatchId", "requestDigest", "claimToken", "leaseEpoch"}
var limits = [5]int{128, 36, 64, 43, 16}

// span is transient integer bookkeeping, never retained by an Observation.
type span struct{ start, end int }
type member struct {
	key    [80]byte
	keyLen int
	value  span
}

// CaptureHTTP observes only the first message used by the existing consumer.
// Call after successful legacy decoding, not as an alternative JSON decoder.
func CaptureHTTP(raw []byte) *Observation {
	return freeze(*captureHTTP(raw))
}

func captureHTTP(raw []byte) *ownedRecord {
	o := empty("redis-api")
	if !json.Valid(raw) {
		return o
	}
	messages, count, ok := find(raw, span{0, len(raw)}, "messages", true)
	if !ok {
		return o
	}
	if count > 1 {
		o.provenance = Repeated
		return o
	}
	if count != 1 {
		return o
	}
	a := trim(raw, messages)
	if a.start >= a.end || raw[a.start] != '[' {
		return o
	}
	first := space(raw, a.start+1)
	if first >= a.end || raw[first] == ']' {
		return o
	}
	end, valid := valueEnd(raw, first)
	if !valid {
		return o
	}
	message := span{first, end}
	data, n, valid := find(raw, message, "data", true)
	if !valid {
		return o
	}
	if n > 1 {
		o.provenance = Repeated
		return o
	}
	if n != 1 || !object(raw, data) {
		return o
	}
	o = captureData(raw, data, true, "redis-api")
	o.identity.MessageID = identity(raw, message, "id", 64, true, true, o)
	return o
}

// CaptureWS observes an original job frame; decoded maps alone are insufficient.
func CaptureWS(raw []byte) *Observation {
	return freeze(*captureWS(raw))
}

func captureWS(raw []byte) *ownedRecord {
	o := empty("websocket")
	if !json.Valid(raw) {
		return o
	}
	root := span{0, len(raw)}
	data, n, ok := find(raw, root, "data", true)
	if !ok {
		return o
	}
	if n > 1 {
		o.provenance = Repeated
		return o
	}
	if n != 1 || !object(raw, data) {
		return o
	}
	o = captureData(raw, data, false, "websocket")
	o.identity.MessageID = identity(raw, root, "id", 64, true, true, o)
	o.identity.Queue = identity(raw, root, "queue", 256, false, true, o)
	frameType := identity(raw, root, "type", 128, false, true, o)
	if frameType != "job" {
		o.identityInvalid = true
	}
	return o
}

func empty(transport string) *ownedRecord {
	o := &ownedRecord{provenance: Uncertain, identity: Identity{Transport: transport}}
	for i := range o.presence {
		o.presence[i] = Missing
	}
	return o
}

func captureData(raw []byte, data span, foldIdentity bool, transport string) *ownedRecord {
	o := empty(transport)
	o.provenance = Original
	ok := members(raw, data, func(m member) {
		for i, key := range names {
			if string(m.key[:m.keyLen]) != key {
				continue
			}
			if o.presence[i] != Missing {
				o.presence[i] = Duplicate
				o.values[i] = ""
				return
			}
			o.values[i], o.presence[i] = boundedString(raw, m.value, limits[i], true)
			return
		}
	})
	if !ok {
		o.provenance = Uncertain
		return o
	}
	o.identity.JobID = identity(raw, data, "jobId", 36, false, foldIdentity, o)
	o.identity.Type = identity(raw, data, "type", 128, false, foldIdentity, o)
	return o
}

func identity(raw []byte, scope span, key string, cap int, ascii, fold bool, o *ownedRecord) string {
	v, n, ok := find(raw, scope, key, fold)
	if !ok {
		o.provenance = Uncertain
		return ""
	}
	if n > 1 {
		o.provenance = Repeated
		return ""
	}
	if n != 1 {
		o.identityInvalid = true
		return ""
	}
	value, p := boundedString(raw, v, cap, ascii)
	if p != Captured || value == "" {
		o.identityInvalid = true
		return ""
	}
	return value
}

func boundedString(raw []byte, s span, cap int, ascii bool) (string, Presence) {
	s = trim(raw, s)
	if s.start >= s.end {
		return "", Invalid
	}
	if s.end-s.start == 4 && string(raw[s.start:s.end]) == "null" {
		return "", Null
	}
	if raw[s.start] != '"' {
		return "", WrongType
	}
	if s.end-s.start > 6*cap+2 {
		return "", EncodedLimit
	}
	var value string
	if json.Unmarshal(raw[s.start:s.end], &value) != nil || len(value) > cap || ascii && !isASCII(value) {
		return "", Invalid
	}
	return strings.Clone(value), Captured
}

func find(raw []byte, scope span, key string, fold bool) (span, int, bool) {
	var found span
	count := 0
	ok := members(raw, scope, func(m member) {
		if string(m.key[:m.keyLen]) == key || fold && strings.EqualFold(string(m.key[:m.keyLen]), key) {
			found = m.value
			count++
		}
	})
	return found, count, ok
}

func object(raw []byte, s span) bool {
	s = trim(raw, s)
	return s.start < s.end && raw[s.start] == '{'
}

// members walks a unique object scope. Unknown keys and nested values are skipped
// without map/RawMessage copies. All selected key decoding is bounded first.
func members(raw []byte, scope span, visit func(member)) bool {
	scope = trim(raw, scope)
	if !object(raw, scope) {
		return false
	}
	p := space(raw, scope.start+1)
	for p < scope.end && raw[p] != '}' {
		if raw[p] != '"' {
			return false
		}
		keyEnd, ok := stringEnd(raw, p)
		if !ok {
			return false
		}
		var m member
		// requestDigest fully escaped is 13*6+2 bytes. Longer encoded
		// keys cannot represent one of our exact or legacy folded names.
		if keyEnd-p <= 80 {
			var valid bool
			m.keyLen, valid = keyName(raw[p:keyEnd], &m.key)
			if !valid {
				return false
			}
		}
		p = space(raw, keyEnd)
		if p >= scope.end || raw[p] != ':' {
			return false
		}
		p = space(raw, p+1)
		end, ok := valueEnd(raw, p)
		if !ok || end > scope.end {
			return false
		}
		m.value = span{p, end}
		if m.keyLen != 0 {
			visit(m)
		}
		p = space(raw, end)
		if p < scope.end && raw[p] == ',' {
			p = space(raw, p+1)
		} else {
			break
		}
	}
	return p < scope.end && raw[p] == '}'
}

func space(raw []byte, p int) int {
	for p < len(raw) && (raw[p] == ' ' || raw[p] == '\t' || raw[p] == '\r' || raw[p] == '\n') {
		p++
	}
	return p
}
func trim(raw []byte, s span) span {
	s.start = space(raw, s.start)
	for s.end > s.start && (raw[s.end-1] == ' ' || raw[s.end-1] == '\t' || raw[s.end-1] == '\r' || raw[s.end-1] == '\n') {
		s.end--
	}
	return s
}
func stringEnd(raw []byte, p int) (int, bool) {
	for p++; p < len(raw); p++ {
		if raw[p] == '\\' {
			p++
			continue
		}
		if raw[p] == '"' {
			return p + 1, true
		}
	}
	return 0, false
}
func valueEnd(raw []byte, p int) (int, bool) {
	if p >= len(raw) {
		return 0, false
	}
	if raw[p] == '"' {
		return stringEnd(raw, p)
	}
	if raw[p] != '{' && raw[p] != '[' {
		end := p
		for end < len(raw) && raw[end] != ',' && raw[end] != '}' && raw[end] != ']' {
			end++
		}
		return end, end > p
	}
	depth := 0
	for ; p < len(raw); p++ {
		switch raw[p] {
		case '"':
			end, ok := stringEnd(raw, p)
			if !ok {
				return 0, false
			}
			p = end - 1
		case '{', '[':
			depth++
			if depth > 1024 {
				return 0, false
			}
		case '}', ']':
			depth--
			if depth == 0 {
				return p + 1, true
			}
		}
	}
	return 0, false
}

// keyName decodes bounded JSON names into stack storage, including escaped
// Unicode SimpleFold aliases. Unknown names allocate no heap strings/maps.
func keyName(raw []byte, out *[80]byte) (int, bool) {
	n := 0
	for p := 1; p < len(raw)-1; {
		var r rune
		if raw[p] != '\\' {
			var size int
			r, size = utf8.DecodeRune(raw[p : len(raw)-1])
			p += size
			if r == utf8.RuneError {
				return 0, false
			}
		} else {
			p++
			if p >= len(raw)-1 {
				return 0, false
			}
			switch raw[p] {
			case '"', '\\', '/':
				r = rune(raw[p])
				p++
			case 'b':
				r = '\b'
				p++
			case 'f':
				r = '\f'
				p++
			case 'n':
				r = '\n'
				p++
			case 'r':
				r = '\r'
				p++
			case 't':
				r = '\t'
				p++
			case 'u':
				var ok bool
				r, ok = hexRune(raw, p+1)
				if !ok {
					return 0, false
				}
				p += 5
				if utf16.IsSurrogate(r) {
					if p+6 > len(raw)-1 || raw[p] != '\\' || raw[p+1] != 'u' {
						return 0, false
					}
					r2, valid := hexRune(raw, p+2)
					if !valid {
						return 0, false
					}
					r = utf16.DecodeRune(r, r2)
					if r == utf8.RuneError {
						return 0, false
					}
					p += 6
				}
			default:
				return 0, false
			}
		}
		if n+utf8.RuneLen(r) > len(out) {
			return 0, false
		}
		n += utf8.EncodeRune(out[n:], r)
	}
	return n, true
}

func hexRune(raw []byte, p int) (rune, bool) {
	if p+4 > len(raw) {
		return 0, false
	}
	var r rune
	for _, c := range raw[p : p+4] {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r += rune(c - '0')
		case c >= 'a' && c <= 'f':
			r += rune(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			r += rune(c - 'A' + 10)
		default:
			return 0, false
		}
	}
	return r, true
}
