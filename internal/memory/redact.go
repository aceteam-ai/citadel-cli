package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const redactedValue = "[REDACTED]"

// AceTeam bearer keys are act_ followed by 64 hex characters today. Keep the
// matcher deliberately a little broader so diagnostics stay safe if the key
// encoding changes before this client is upgraded.
var aceTeamBearerPattern = regexp.MustCompile(`act_[0-9A-Za-z._-]{8,}`)

// RedactSensitiveText removes explicitly supplied credentials/session tokens
// and any AceTeam bearer-shaped value from text that may reach stdout, stderr,
// or a JSON-RPC client. It is intentionally exported for the stdio MCP bridge,
// which lives in cmd and must scrub untrusted backend diagnostics too.
func RedactSensitiveText(text string, sensitive ...string) string {
	for _, value := range sensitive {
		if value != "" {
			text = strings.ReplaceAll(text, value, redactedValue)
		}
	}
	return aceTeamBearerPattern.ReplaceAllString(text, redactedValue)
}

// RedactSensitiveJSON decodes and re-encodes one JSON value while redacting
// every string key/value. Structural decoding is important because a hostile
// backend can spell a bearer using JSON unicode escapes that are invisible to
// a byte-level replacement but become the raw secret at the stdio client.
func RedactSensitiveJSON(data []byte, sensitive ...string) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode JSON for redaction: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("JSON for redaction contains trailing data")
	}
	return json.Marshal(redactJSONValue(value, sensitive))
}

func redactJSONValue(value any, sensitive []string) any {
	switch typed := value.(type) {
	case string:
		return RedactSensitiveText(typed, sensitive...)
	case []any:
		for i := range typed {
			typed[i] = redactJSONValue(typed[i], sensitive)
		}
		return typed
	case map[string]any:
		redacted := make(map[string]any, len(typed))
		for key, child := range typed {
			redacted[RedactSensitiveText(key, sensitive...)] = redactJSONValue(child, sensitive)
		}
		return redacted
	default:
		return value
	}
}
