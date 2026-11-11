// Package definition pins the source-only deployment digest contract. It does
// not enroll, persist, dispatch or start a stream, nor does a digest authorize
// reading a path or forwarding evidence.
package definition

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/gowebpki/jcs"
)

const Domain = "stream.definition.v1"
const MaxBytes = 256 << 10

var errDefinition = errors.New("invalid fully materialized stream definition")

// Canonical returns RFC8785 bytes of exactly {definition, organization_id}.
// organizationID must come from authenticated enrollment, not incoming job data.
// status and other mutable deployment metadata are deliberately not arguments.
func Canonical(organizationID string, materialized []byte) ([]byte, error) {
	if !canonicalUUID(organizationID) || len(materialized) == 0 || len(materialized) > MaxBytes || !utf8.Valid(materialized) {
		return nil, errDefinition
	}
	// Bound structure before handing input to the canonicalizer. Check raw
	// surrogate escapes ourselves: neither encoding/json nor the pinned JCS
	// library rejects every malformed surrogate pair without replacement.
	if !boundedJSON(materialized) || !validUnicodeEscapes(materialized) {
		return nil, errDefinition
	}
	if _, err := jcs.Transform(materialized); err != nil {
		return nil, errDefinition
	}
	decoder := json.NewDecoder(bytes.NewReader(materialized))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil || !validDefinition(value) {
		return nil, errDefinition
	}
	wrapped, err := json.Marshal(map[string]any{"definition": value, "organization_id": organizationID})
	if err != nil {
		return nil, errDefinition
	}
	canonical, err := jcs.Transform(wrapped)
	if err != nil {
		return nil, errDefinition
	}
	return canonical, nil
}

func validUnicodeEscapes(data []byte) bool {
	inString := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			return false
		}
		if data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return false
		}
		first, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if first >= 0xd800 && first <= 0xdbff {
			if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return false
			}
			second, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err != nil || second < 0xdc00 || second > 0xdfff {
				return false
			}
			i += 6
		} else if first >= 0xdc00 && first <= 0xdfff {
			return false
		}
	}
	return !inString
}

// Digest hashes Domain, one zero byte, then Canonical's bytes. Nothing here
// accepts a caller's asserted digest as proof of review or deployment authority.
func Digest(organizationID string, materialized []byte) (string, error) {
	canonical, err := Canonical(organizationID, materialized)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(Domain))
	h.Write([]byte{0})
	h.Write(canonical)
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func boundedJSON(data []byte) bool {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	depth, tokens := 0, 0
	for {
		token, err := d.Token()
		if err == io.EOF {
			return depth == 0 && tokens > 0
		}
		if err != nil {
			return false
		}
		tokens++
		if tokens > 16384 {
			return false
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
			if depth > 16 || depth < 0 {
				return false
			}
		}
	}
}

func object(value any, keys ...string) (map[string]any, bool) {
	m, ok := value.(map[string]any)
	if !ok || len(m) != len(keys) {
		return nil, false
	}
	for _, key := range keys {
		if _, exists := m[key]; !exists {
			return nil, false
		}
	}
	return m, true
}

func validDefinition(value any) bool {
	d, ok := object(value, "stream_id", "version", "node_id", "sources", "pack", "escalate", "evidence", "time", "synthetic")
	if !ok || !identifier(d["stream_id"]) || !integer(d["version"], 1, 9007199254740991) || !nodeID(d["node_id"]) || d["time"] != "ingest" || d["synthetic"] != true {
		return false
	}
	sources, ok := d["sources"].([]any)
	if !ok || len(sources) < 1 || len(sources) > 16 {
		return false
	}
	seen := map[string]bool{}
	for _, source := range sources {
		s, ok := object(source, "id", "type", "parser", "path", "bind", "port")
		if !ok || !identifier(s["id"]) {
			return false
		}
		id := s["id"].(string)
		if seen[id] {
			return false
		}
		seen[id] = true
		switch s["parser"] {
		case "openssh", "combined", "json", "jsonl", "syslog", "syslog3164", "syslog5424":
		default:
			return false
		}
		switch s["type"] {
		case "file":
			path, ok := s["path"].(string)
			if !ok || len(path) > 4096 || !strings.HasPrefix(path, "/") || strings.ContainsRune(path, 0) || s["bind"] != nil || s["port"] != nil {
				return false
			}
			for _, part := range strings.Split(path, "/") {
				if part == ".." {
					return false
				}
			}
		case "syslog_udp", "syslog_tcp":
			bind, ok := s["bind"].(string)
			if !ok || bind == "" || len(bind) > 256 || strings.ContainsRune(bind, 0) || s["path"] != nil || !integer(s["port"], 1024, 65535) {
				return false
			}
		default:
			return false
		}
	}
	p, ok := object(d["pack"], "id", "version", "digest")
	if !ok || !identifier(p["id"]) || !identifier(p["version"]) || !digestString(p["digest"]) {
		return false
	}
	e, ok := object(d["evidence"], "mode", "max_lines", "max_bytes_per_line")
	if !ok || (e["mode"] != "digest" && e["mode"] != "sampled") || !integer(e["max_lines"], 0, 5) || !integer(e["max_bytes_per_line"], 0, 2048) {
		return false
	}
	c, ok := object(d["escalate"], "flow_id", "flow_version_id", "input_map", "max_per_hour", "max_concurrent", "max_run_cost_micro_usd", "daily_cap_micro_usd")
	if !ok || !uuidValue(c["flow_id"]) || !uuidValue(c["flow_version_id"]) || !integer(c["max_per_hour"], 1, 60) || !integer(c["max_concurrent"], 1, 2) || !integer(c["max_run_cost_micro_usd"], 1, 1000000) || !integer(c["daily_cap_micro_usd"], 1, 5000000) {
		return false
	}
	inputs, ok := c["input_map"].(map[string]any)
	if !ok || len(inputs) < 1 || len(inputs) > 64 {
		return false
	}
	for name, mapping := range inputs {
		if name == "" || len([]rune(name)) > 128 {
			return false
		}
		switch mapping {
		case "$alert", "$evidence", "$evidence.samples", "$evidence.digests", "$match_count", "$logs":
		default:
			return false
		}
	}
	return true
}

func identifier(value any) bool {
	s, ok := value.(string)
	if !ok || len(s) < 1 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_.-", c)) {
			return false
		}
	}
	return true
}

func canonicalUUID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u != uuid.Nil && u.String() == s
}
func uuidValue(v any) bool { s, ok := v.(string); return ok && canonicalUUID(s) }

func nodeID(value any) bool {
	s, ok := value.(string)
	if !ok || s == "" || s[0] < '1' || s[0] > '9' {
		return false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return err == nil && strconv.FormatUint(n, 10) == s
}

func digestString(value any) bool {
	s, ok := value.(string)
	if !ok || len(s) != 71 || !strings.HasPrefix(s, "sha256:") || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s[7:])
	return err == nil
}

func integer(value any, minimum, maximum int64) bool {
	n, ok := value.(json.Number)
	if !ok {
		return false
	}
	i, err := n.Int64()
	return err == nil && strconv.FormatInt(i, 10) == n.String() && i >= minimum && i <= maximum
}
