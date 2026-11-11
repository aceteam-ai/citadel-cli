package event

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var combinedLine = regexp.MustCompile(`^(\S+) (\S+) (\S+) \[([^\]]+)\] "((?:\\.|[^"\\])*)" ([0-9]{3}) ([0-9]+|-) "((?:\\.|[^"\\])*)" "((?:\\.|[^"\\])*)"$`)
var requestMethod = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
var requestProtocol = regexp.MustCompile(`^HTTP/[0-9]+\.[0-9]+$`)

func parseCombined(line string) (map[string]any, error) {
	parts := combinedLine.FindStringSubmatch(line)
	if parts == nil {
		return nil, errors.New("malformed combined access log")
	}
	ip, err := parseIP(parts[1])
	if err != nil {
		return nil, err
	}
	timestamp, err := time.Parse("02/Jan/2006:15:04:05 -0700", parts[4])
	if err != nil {
		return nil, errors.New("invalid access log timestamp")
	}
	request, err := unescapeAccess(parts[5])
	if err != nil {
		return nil, err
	}
	requestParts := strings.Split(request, " ")
	if len(requestParts) != 3 || !requestMethod.MatchString(requestParts[0]) ||
		requestParts[1] == "" || !requestProtocol.MatchString(requestParts[2]) {
		return nil, errors.New("invalid access log request line")
	}
	status, err := parseInteger(parts[6], 100, 599)
	if err != nil {
		return nil, errors.New("invalid HTTP response status")
	}
	referrer, err := unescapeAccess(parts[8])
	if err != nil {
		return nil, err
	}
	userAgent, err := unescapeAccess(parts[9])
	if err != nil {
		return nil, err
	}
	// Preserve the request target byte-for-byte. In particular, do not decode
	// %2f, collapse path segments, or lower-case mixed-case paths before rules
	// have a chance to select their own normalization semantics.
	target := requestParts[1]
	path := strings.SplitN(target, "?", 2)[0]
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		// Query bytes are not an authority or a path. A slash in a redirect
		// parameter of http://host?next=/admin must not become url.path.
		start := strings.Index(path, "://") + 3
		if slash := strings.IndexByte(path[start:], '/'); slash >= 0 {
			path = path[start+slash:]
		} else {
			path = "/"
		}
	}
	fields := map[string]any{
		"@timestamp":                timestamp.UTC().Format(time.RFC3339Nano),
		"event.category":            "web",
		"event.action":              "http_request",
		"source.ip":                 ip,
		"http.request.method":       requestParts[0],
		"http.request.line":         request,
		"http.version":              strings.TrimPrefix(requestParts[2], "HTTP/"),
		"http.response.status_code": status,
		"url.original":              target,
		"url.path":                  path,
		"user_agent.original":       userAgent,
		"message":                   line,
	}
	if parts[3] != "-" {
		fields["user.name"] = parts[3]
	}
	if referrer != "-" {
		fields["http.request.referrer"] = referrer
	}
	if parts[7] != "-" {
		bytes, countErr := parseInteger(parts[7], 0, 1<<63-1)
		if countErr != nil {
			return nil, errors.New("invalid access log byte count")
		}
		fields["http.response.body.bytes"] = bytes
		fields["network.bytes"] = bytes
	}
	return fields, nil
}

// nginx and Apache escape quotes, backslashes, and non-printing bytes in
// quoted fields. Refuse unknown escapes instead of silently losing bytes.
func unescapeAccess(value string) (string, error) {
	var output strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			output.WriteByte(value[i])
			continue
		}
		i++
		if i == len(value) {
			return "", errors.New("unterminated access log escape")
		}
		switch value[i] {
		case '\\', '"':
			output.WriteByte(value[i])
		case 'n':
			output.WriteByte('\n')
		case 'r':
			output.WriteByte('\r')
		case 't':
			output.WriteByte('\t')
		case 'b':
			output.WriteByte('\b')
		case 'v':
			output.WriteByte('\v')
		case 'x':
			if i+2 >= len(value) {
				return "", errors.New("incomplete access log byte escape")
			}
			number, err := strconv.ParseUint(value[i+1:i+3], 16, 8)
			if err != nil {
				return "", errors.New("invalid access log byte escape")
			}
			output.WriteByte(byte(number))
			i += 2
		default:
			return "", errors.New("unsupported access log escape")
		}
	}
	return output.String(), nil
}
