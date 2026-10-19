package event

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

var header3164 = regexp.MustCompile(`^([A-Z][a-z]{2} [ 0-9][0-9] [0-9]{2}:[0-9]{2}:[0-9]{2}) ([^ ]+) (.+)$`)
var programTag = regexp.MustCompile(`^([^ :\[\]]+)(?:\[([0-9]+)\])?: ?(.*)$`)
var isoSyslogHeader = regexp.MustCompile(`^([^ ]+T[^ ]+) ([^ ]+) (.+)$`)

func parseSyslog(kind, line string, meta Envelope) (map[string]any, error) {
	fields := make(map[string]any)
	body := line
	if strings.HasPrefix(body, "<") {
		end := strings.IndexByte(body, '>')
		if end < 2 || end > 4 {
			return nil, errors.New("invalid syslog priority")
		}
		priority, err := parseInteger(body[1:end], 0, 191)
		if err != nil {
			return nil, errors.New("invalid syslog priority")
		}
		fields["log.syslog.priority"] = priority
		fields["log.syslog.facility.code"] = priority / 8
		fields["log.syslog.severity.code"] = priority % 8
		body = body[end+1:]
	} else if kind == "syslog5424" {
		return nil, errors.New("RFC5424 syslog requires a priority header")
	}
	// RFC5424 begins with a numeric version, unlike the RFC3164 month.
	is5424 := len(body) > 0 && body[0] >= '0' && body[0] <= '9'
	if kind == "syslog5424" || (kind == "syslog" && is5424) {
		if _, exists := fields["log.syslog.priority"]; !exists {
			return nil, errors.New("RFC5424 syslog requires a priority header")
		}
		return parse5424(body, fields)
	}
	parts := header3164.FindStringSubmatch(body)
	if parts == nil {
		return nil, errors.New("malformed RFC3164 syslog header")
	}
	if !printableHeader(parts[2], 255) {
		return nil, errors.New("invalid RFC3164 hostname")
	}
	if meta.TWallNS == 0 {
		return nil, errors.New("RFC3164 timestamp requires the ingest wall clock")
	}
	// RFC3164 has neither year nor zone. Use UTC and the year nearest the
	// supplied ingest stamp (including the adjacent years at New Year), never
	// this process's wall clock or machine-local timezone.
	ingest := time.Unix(0, meta.TWallNS).UTC()
	var timestamp time.Time
	var distance time.Duration
	for year := ingest.Year() - 1; year <= ingest.Year()+1; year++ {
		candidate, err := time.Parse("2006 Jan _2 15:04:05", yearString(year)+" "+parts[1])
		if err != nil {
			continue
		}
		delta := candidate.Sub(ingest)
		if delta < 0 {
			delta = -delta
		}
		if timestamp.IsZero() || delta < distance {
			timestamp, distance = candidate, delta
		}
	}
	if timestamp.IsZero() {
		return nil, errors.New("invalid RFC3164 timestamp")
	}
	fields["@timestamp"] = timestamp.Format(time.RFC3339Nano)
	fields["host.name"] = parts[2]
	if err := addProgramMessage(parts[3], fields); err != nil {
		return nil, err
	}
	return fields, nil
}

func yearString(year int) string {
	return time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).Format("2006")
}

func addProgramMessage(message string, fields map[string]any) error {
	parts := programTag.FindStringSubmatch(message)
	if parts == nil {
		fields["message"] = message
		return nil
	}
	fields["process.name"] = parts[1]
	if parts[2] != "" {
		pid, err := parseInteger(parts[2], 0, 1<<31-1)
		if err != nil {
			return errors.New("invalid syslog process identifier")
		}
		fields["process.pid"] = pid
	}
	fields["message"] = parts[3]
	return nil
}

func parse5424(body string, fields map[string]any) (map[string]any, error) {
	parts := strings.SplitN(body, " ", 7)
	if len(parts) != 7 {
		return nil, errors.New("malformed RFC5424 syslog header")
	}
	if parts[0] != "1" {
		return nil, errors.New("unsupported RFC5424 version")
	}
	if parts[1] != "-" {
		stamp, err := time.Parse(time.RFC3339Nano, parts[1])
		if err != nil {
			return nil, errors.New("invalid RFC5424 timestamp")
		}
		fields["@timestamp"] = stamp.UTC().Format(time.RFC3339Nano)
	}
	for i, maximum := range []int{255, 48, 128, 32} {
		if !printableHeader(parts[i+2], maximum) {
			return nil, errors.New("invalid RFC5424 header field")
		}
	}
	if parts[2] != "-" {
		fields["host.name"] = parts[2]
	}
	if parts[3] != "-" {
		fields["process.name"] = parts[3]
	}
	if parts[4] != "-" {
		fields["log.syslog.procid"] = parts[4]
		if pid, err := parseInteger(parts[4], 0, 1<<31-1); err == nil {
			fields["process.pid"] = pid
		}
	}
	if parts[5] != "-" {
		fields["log.syslog.msgid"] = parts[5]
	}
	structured, message, err := splitStructuredData(parts[6])
	if err != nil {
		return nil, err
	}
	if structured != "-" {
		fields["log.syslog.structured_data"] = structured
	}
	fields["message"] = strings.TrimPrefix(message, "\ufeff")
	return fields, nil
}

func printableHeader(value string, maximum int) bool {
	if len(value) == 0 || len(value) > maximum {
		return false
	}
	for _, char := range value {
		if char < 33 || char > 126 {
			return false
		}
	}
	return true
}

func validSDName(value string) bool {
	return printableHeader(value, 32) && !strings.ContainsAny(value, `="]`)
}

// Validate RFC5424 structured data without interpreting its values as code or
// flattening attacker-controlled names into the ECS namespace.
func splitStructuredData(value string) (string, string, error) {
	bad := errors.New("invalid RFC5424 structured data")
	if value == "-" {
		return "-", "", nil
	}
	if strings.HasPrefix(value, "- ") {
		return "-", value[2:], nil
	}
	position := 0
	ids := make(map[string]bool)
	for position < len(value) && value[position] == '[' {
		position++
		start := position
		for position < len(value) && value[position] != ' ' && value[position] != ']' {
			position++
		}
		id := value[start:position]
		if !validSDName(id) || ids[id] {
			return "", "", bad
		}
		ids[id] = true
		params := make(map[string]bool)
		for position < len(value) && value[position] == ' ' {
			position++
			start = position
			for position < len(value) && value[position] != '=' {
				position++
			}
			name := value[start:position]
			if !validSDName(name) || params[name] || position+1 >= len(value) || value[position+1] != '"' {
				return "", "", bad
			}
			params[name] = true
			position += 2
			closed := false
			for position < len(value) {
				char := value[position]
				position++
				if char == '"' {
					closed = true
					break
				}
				if char == ']' {
					return "", "", bad
				}
				if char == '\\' {
					if position == len(value) || !strings.ContainsRune(`\"]`, rune(value[position])) {
						return "", "", bad
					}
					position++
				}
			}
			if !closed {
				return "", "", bad
			}
		}
		if position == len(value) || value[position] != ']' {
			return "", "", bad
		}
		position++
	}
	if position == 0 {
		return "", "", bad
	}
	if position == len(value) {
		return value, "", nil
	}
	if value[position] != ' ' {
		return "", "", bad
	}
	return value[:position], value[position+1:], nil
}
