package event

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var fieldName = regexp.MustCompile(`^(@timestamp|[a-zA-Z_][a-zA-Z0-9_.-]*)$`)

func parseJSON(line string, fieldMap map[string]string) (map[string]any, error) {
	if len(fieldMap) == 0 || len(fieldMap) > 256 {
		return nil, errors.New("JSON parser requires a bounded declared field map")
	}
	decoder := json.NewDecoder(strings.NewReader(line))
	decoder.UseNumber()
	budget := 16384
	value, err := decodeJSON(decoder, 0, &budget)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("JSON event has trailing data")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("JSON event must be an object")
	}
	fields := map[string]any{"message": line}
	outputs := make(map[string]bool)
	keys := make([]string, 0, len(fieldMap))
	for input := range fieldMap {
		keys = append(keys, input)
	}
	sort.Strings(keys)
	for _, input := range keys {
		output := fieldMap[input]
		if input == "" || len(input) > 256 || len(output) > 128 || !fieldName.MatchString(output) || outputs[output] {
			return nil, errors.New("invalid or duplicate JSON output field")
		}
		outputs[output] = true
		mapped, found := mappedJSONField(object, input)
		if !found {
			return nil, errors.New("declared JSON input field is missing")
		}
		mapped, err = scalarJSONValue(mapped)
		if err != nil {
			return nil, err
		}
		switch output {
		case "source.ip", "destination.ip":
			ip, isString := mapped.(string)
			if !isString {
				return nil, errors.New("mapped IP field must be a string")
			}
			mapped, err = parseIP(ip)
		case "@timestamp":
			stamp, isString := mapped.(string)
			if !isString {
				return nil, errors.New("mapped event timestamp must be RFC3339")
			}
			var parsed time.Time
			parsed, err = time.Parse(time.RFC3339Nano, stamp)
			if err == nil {
				mapped = parsed.UTC().Format(time.RFC3339Nano)
			}
		case "destination.port", "source.port", "http.response.status_code":
			number, isInteger := mapped.(int64)
			minimum, maximum := int64(0), int64(65535)
			if output == "http.response.status_code" {
				minimum, maximum = 100, 599
			}
			if !isInteger || number < minimum || number > maximum {
				return nil, errors.New("mapped numeric protocol field is outside its range")
			}
		}
		if err != nil {
			return nil, errors.New("invalid mapped event field")
		}
		fields[output] = mapped
	}
	return fields, nil
}

func mappedJSONField(object map[string]any, path string) (any, bool) {
	if value, found := object[path]; found {
		return value, true
	}
	parts := strings.Split(path, ".")
	var current any = object
	for _, part := range parts {
		next, ok := current.(map[string]any)
		if !ok || part == "" {
			return nil, false
		}
		current, ok = next[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func scalarJSONValue(value any) (any, error) {
	switch typed := value.(type) {
	case nil, string, bool:
		return value, nil
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			return integer, nil
		}
		if !strings.ContainsAny(string(typed), ".eE") {
			return nil, errors.New("JSON integer is outside the supported range")
		}
		number, err := strconv.ParseFloat(string(typed), 64)
		if err != nil || math.IsInf(number, 0) || math.IsNaN(number) {
			return nil, errors.New("JSON number is outside the supported range")
		}
		return number, nil
	case []any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			if _, nested := item.([]any); nested {
				return nil, errors.New("mapped JSON arrays must contain scalar values")
			}
			scalar, err := scalarJSONValue(item)
			if err != nil {
				return nil, err
			}
			result = append(result, scalar)
		}
		return result, nil
	default:
		return nil, errors.New("mapped JSON field must be scalar or a scalar array")
	}
}

// Decode through tokens rather than Unmarshal so duplicate keys (including
// escaped spellings and duplicates in unmapped objects) never overwrite data.
func decodeJSON(decoder *json.Decoder, depth int, budget *int) (any, error) {
	if depth > 16 || *budget <= 0 {
		return nil, errors.New("JSON event exceeds structural limits")
	}
	*budget -= 1
	token, err := decoder.Token()
	if err != nil {
		return nil, errors.New("malformed JSON event")
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			key, keyOK := keyToken.(string)
			if keyErr != nil || !keyOK {
				return nil, errors.New("malformed JSON object key")
			}
			if _, duplicate := object[key]; duplicate {
				return nil, errors.New("duplicate JSON object key")
			}
			value, valueErr := decodeJSON(decoder, depth+1, budget)
			if valueErr != nil {
				return nil, valueErr
			}
			object[key] = value
		}
		end, endErr := decoder.Token()
		if endErr != nil || end != json.Delim('}') {
			return nil, errors.New("unterminated JSON object")
		}
		return object, nil
	case '[':
		var array []any
		for decoder.More() {
			value, valueErr := decodeJSON(decoder, depth+1, budget)
			if valueErr != nil {
				return nil, valueErr
			}
			array = append(array, value)
		}
		end, endErr := decoder.Token()
		if endErr != nil || end != json.Delim(']') {
			return nil, errors.New("unterminated JSON array")
		}
		return array, nil
	default:
		return nil, errors.New("unexpected JSON delimiter")
	}
}
