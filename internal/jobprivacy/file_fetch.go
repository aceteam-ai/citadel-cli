// Package jobprivacy keeps delivery credentials out of retained job diagnostics.
package jobprivacy

import "strings"

const FileFetch = "FILE_FETCH"

// Payload is for retention/diagnostics only, never execution or redispatch.
func Payload(kind string, payload map[string]any) map[string]any {
	if kind == FileFetch {
		return nil
	}
	return payload
}

// Text removes a reflected delivery credential while preserving safe reasons.
func Text(kind string, payload map[string]any, text string) string {
	if kind == FileFetch {
		if token, ok := payload["capability"].(string); ok && token != "" {
			return strings.ReplaceAll(text, token, "[REDACTED]")
		}
	}
	return text
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
	if kind != FileFetch {
		for key, value := range data {
			fields[key] = value
		}
	}
	return fields
}
