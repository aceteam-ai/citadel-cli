package pulse

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// promSample is one parsed Prometheus exposition sample: bare metric name,
// label set, and value.
//
// The status package's idle tracker (internal/status/idle.go) already has a
// minimal exposition parser, but it deliberately discards labels. Pulse needs
// them — histogram buckets are keyed by the "le" label and the served model
// rides the "model_name" label — so this file carries its own label-aware
// parser instead of widening that one.
type promSample struct {
	name   string
	labels map[string]string
	value  float64
}

// parsePromText reads a Prometheus text-format exposition and returns all
// samples it can parse, skipping comments, blanks, and malformed lines. It is
// tolerant by design: a partially garbled exposition yields the parseable
// subset rather than an error, matching the "absent/partial metrics are not an
// error" contract.
func parsePromText(r io.Reader) []promSample {
	var out []promSample
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if s, ok := parsePromLine(line); ok {
			out = append(out, s)
		}
	}
	return out
}

// parsePromLine parses a single exposition sample line:
//
//	metric_name{label="value",...} value [timestamp]
//
// Label values may contain escaped quotes (\") and any byte except an
// unescaped quote, including spaces, commas, and braces.
func parsePromLine(line string) (promSample, bool) {
	name, rest, labels, ok := splitSeries(line)
	if !ok {
		return promSample{}, false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return promSample{}, false
	}
	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return promSample{}, false
	}
	return promSample{name: name, labels: labels, value: value}, true
}

// splitSeries splits "name{labels} rest" into the bare name, the remainder
// after the series token, and the parsed label map (nil when unlabeled).
func splitSeries(line string) (name, rest string, labels map[string]string, ok bool) {
	brace := strings.IndexByte(line, '{')
	// Unlabeled series: name and value split on whitespace.
	if brace < 0 || strings.IndexFunc(line[:brace], isSpace) >= 0 {
		sp := strings.IndexFunc(line, isSpace)
		if sp <= 0 {
			return "", "", nil, false
		}
		return line[:sp], line[sp:], nil, true
	}

	name = line[:brace]
	labels = make(map[string]string)
	i := brace + 1
	for i < len(line) {
		// Closing brace ends the label set.
		if line[i] == '}' {
			return name, line[i+1:], labels, true
		}
		// Parse label name up to '='.
		eq := strings.IndexByte(line[i:], '=')
		if eq < 0 {
			return "", "", nil, false
		}
		key := strings.TrimSpace(line[i : i+eq])
		i += eq + 1
		if i >= len(line) || line[i] != '"' {
			return "", "", nil, false
		}
		i++ // past opening quote
		var val strings.Builder
		for i < len(line) {
			c := line[i]
			if c == '\\' && i+1 < len(line) {
				// Prometheus escapes: \" \\ \n
				next := line[i+1]
				switch next {
				case 'n':
					val.WriteByte('\n')
				default:
					val.WriteByte(next)
				}
				i += 2
				continue
			}
			if c == '"' {
				i++
				break
			}
			val.WriteByte(c)
			i++
		}
		labels[key] = val.String()
		// Skip a separating comma (and any spaces).
		for i < len(line) && (line[i] == ',' || line[i] == ' ') {
			i++
		}
	}
	return "", "", nil, false
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' }
