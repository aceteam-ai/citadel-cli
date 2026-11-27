// Package jobprivacy keeps delivery credentials out of retained job diagnostics.
package jobprivacy

import "strings"

const FileFetch = "FILE_FETCH"

// LaneOperation's send payload carries literal text for an agent pane
// (citadel-cli#1659). It is user content, so it is dropped from every retained
// copy of that job type, like a credential.
const LaneOperation = "LANE_OPERATION"

// LaneMessageKey is the LANE_OPERATION field holding that text.
const LaneMessageKey = "message"

// Retained returns the payload a retained rendering (DLQ, logs, Job JSON and
// fmt output) may keep, and whether anything was dropped. When changed is
// true the caller must also drop the raw stream bytes, which hold the same
// fields. FILE_FETCH is handled separately: its whole payload is withheld.
func Retained(kind string, payload map[string]any) (map[string]any, bool) {
	if kind == LaneOperation {
		if _, ok := payload[LaneMessageKey]; ok {
			out := make(map[string]any, len(payload))
			for k, v := range WithoutPasscode(payload) {
				if k != LaneMessageKey {
					out[k] = v
				}
			}
			return out, true
		}
	}
	if HasPasscode(payload) {
		return WithoutPasscode(payload), true
	}
	return payload, false
}

// Payload is for retention/diagnostics only, never execution or redispatch.
// It drops a FILE_FETCH payload entirely and removes the node passcode field
// from every other job type.
func Payload(kind string, payload map[string]any) map[string]any {
	if kind == FileFetch {
		return nil
	}
	retained, _ := Retained(kind, payload)
	return retained
}

// Text removes a reflected delivery credential or node passcode while
// preserving safe reasons.
func Text(kind string, payload map[string]any, text string) string {
	if kind == FileFetch {
		if token, ok := payload["capability"].(string); ok && token != "" {
			text = replaceAll(text, token)
		}
	}
	if kind == LaneOperation {
		if message, ok := payload[LaneMessageKey].(string); ok && message != "" {
			text = replaceAll(text, message)
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
	if kind != FileFetch {
		for key, value := range data {
			fields[key] = value
		}
	}
	return fields
}
