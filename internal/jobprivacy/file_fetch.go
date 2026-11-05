// Package jobprivacy keeps delivery credentials out of retained job diagnostics.
package jobprivacy

import "strings"

const FileFetch = "FILE_FETCH"
const AMTKVM = "AMT_KVM"

// OmitPayload reports jobs whose entire payload is a delivery credential or
// contains one. Execution keeps the original payload; retention does not.
func OmitPayload(kind string) bool { return kind == FileFetch || kind == AMTKVM }

// Payload is for retention/diagnostics only, never execution or redispatch.
// It drops FILE_FETCH and AMT_KVM payloads entirely and removes the node
// passcode field from every other job type.
func Payload(kind string, payload map[string]any) map[string]any {
	if OmitPayload(kind) {
		return nil
	}
	return WithoutPasscode(payload)
}

// Text removes a reflected delivery credential or node passcode while
// preserving safe reasons.
func Text(kind string, payload map[string]any, text string) string {
	if kind == FileFetch {
		if token, ok := payload["capability"].(string); ok && token != "" {
			text = replaceAll(text, token)
		}
	}
	if kind == AMTKVM {
		for _, key := range []string{"credential_token", "credential_url"} {
			if value, ok := payload[key].(string); ok && value != "" {
				text = replaceAll(text, value)
			}
		}
	}
	return redactPasscode(payload, text)
}

func replaceAll(text, secret string) string {
	return strings.ReplaceAll(text, secret, "[REDACTED]")
}

type redactedError struct {
	original error
	text     string
}

func (e *redactedError) Error() string { return e.text }
func (e *redactedError) Unwrap() error { return e.original }

// Error preserves classification/unwrapping and exposes only redacted text.
func Error(kind string, payload map[string]any, err error) error {
	if err == nil {
		return nil
	}
	if safe := Text(kind, payload, err.Error()); safe != err.Error() {
		return &redactedError{original: err, text: safe}
	}
	return err
}

// FailureData keeps only a sanitized error for credential-bearing jobs.
func FailureData(kind string, payload map[string]any, err error, data map[string]any) map[string]any {
	message := "job failed"
	if err != nil {
		message = Error(kind, payload, err).Error()
	}
	fields := map[string]any{"error": message}
	if !OmitPayload(kind) {
		for key, value := range data {
			fields[key] = value
		}
	}
	return fields
}
