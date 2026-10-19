// Package event normalizes bounded offline log records. It does not read the
// clock, resolve hosts, open sources, or perform any other external operation.
package event

import (
	"errors"
	"net/netip"
	"strings"
	"unicode/utf8"
)

const (
	DefaultMaxLineBytes = 64 * 1024
	HardMaxLineBytes = 1024 * 1024
	ParseStatusOK = "ok"
	ParseStatusError = "error"
)

// Envelope is stamped by the caller at ingest. Event timestamps never replace
// these values, including when a sender's clock is missing or skewed.
type Envelope struct {
	Seq uint64 `json:"seq"`
	TMonoNS int64 `json:"t_mono_ns"`
	TWallNS int64 `json:"t_wall_ns"`
	ClockState string `json:"clock_state"`
	SourceID string `json:"source_id"`
	NodeID string `json:"node_id"`
	RawRef string `json:"raw_ref"`
	ParseStatus string `json:"parse_status"`
}

// Event keeps evidence bytes out of ordinary JSON output. Evidence policy, not
// normalization, decides whether raw input may leave its source.
type Event struct {
	Envelope
	Fields map[string]any `json:"fields"`
	Raw string `json:"-"`
}

// ParserOptions declares all optional normalization. FieldMap is input JSON
// key/path to output ECS dotted field name. An exact literal JSON key takes
// precedence over nested path traversal. Missing mapped keys are errors.
type ParserOptions struct {
	FieldMap map[string]string `json:"field_map,omitempty"`
	Enrichments []string `json:"enrichments,omitempty"`
	MaxLineBytes int `json:"max_line_bytes,omitempty"`
}

var ErrOversize = errors.New("event exceeds line byte limit")

// Parse returns a failed envelope and a non-nil error on every malformed or
// unsupported record. The source driver owns dead-letter persistence/counters.
// Error messages deliberately do not quote raw input or mapped values.
func Parse(kind, raw string, meta Envelope, options ParserOptions) (Event, error) {
	meta.ParseStatus = ParseStatusError
	e := Event{Envelope: meta, Raw: raw}
	limit := options.MaxLineBytes
	if limit == 0 {
		limit = DefaultMaxLineBytes
	}
	if limit < 1 || limit > HardMaxLineBytes {
		return e, errors.New("invalid parser line byte limit")
	}
	if len(raw) > limit {
		return e, ErrOversize
	}
	line := strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\r")
	if line == "" || strings.ContainsAny(line, "\r\n\x00") || !utf8.ValidString(line) {
		return e, errors.New("invalid single-line event")
	}
	if kind != "json" && kind != "jsonl" && len(options.FieldMap) != 0 {
		return e, errors.New("field map is supported only by JSON parsers")
	}
	if len(options.Enrichments) > 16 || len(options.FieldMap) > 256 {
		return e, errors.New("parser options exceed structural limits")
	}
	seen := make(map[string]bool)
	for _, name := range options.Enrichments {
		if name != "source.ip_subnet24" || seen[name] {
			return e, errors.New("unsupported or duplicate event enrichment")
		}
		seen[name] = true
	}
	var fields map[string]any
	var err error
	switch kind {
	case "syslog", "syslog3164", "syslog5424":
		fields, err = parseSyslog(kind, line, meta)
	case "openssh":
		fields, err = parseOpenSSH(line, meta)
	case "combined":
		fields, err = parseCombined(line)
	case "json", "jsonl":
		fields, err = parseJSON(line, options.FieldMap)
	default:
		err = errors.New("unsupported event parser")
	}
	if err != nil {
		return e, err
	}
	if seen["source.ip_subnet24"] {
		value, exists := fields["source.ip"]
		if exists {
			ip, ok := value.(string)
			address, ipErr := netip.ParseAddr(ip)
			if !ok || ipErr != nil || !address.Is4() {
				return e, errors.New("subnet24 enrichment requires an IPv4 source address")
			}
			fields["source.ip_subnet24"] = netip.PrefixFrom(address, 24).Masked().String()
		}
	}
	e.ParseStatus = ParseStatusOK
	e.Fields = fields
	return e, nil
}

func parseIP(value string) (string, error) {
	ip, err := netip.ParseAddr(value)
	if err != nil || ip.Zone() != "" {
		return "", errors.New("invalid source IP address")
	}
	return ip.String(), nil
}

func parseInteger(value string, minimum, maximum int64) (int64, error) {
	var number int64
	if value == "" {
		return 0, errors.New("missing integer field")
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' || number > (maximum-int64(digit-'0'))/10 {
			return 0, errors.New("invalid integer field")
		}
		number = number*10 + int64(digit-'0')
	}
	if number < minimum || number > maximum {
		return 0, errors.New("integer field outside allowed range")
	}
	return number, nil
}
