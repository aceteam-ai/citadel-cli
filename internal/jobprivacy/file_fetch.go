// Package jobprivacy keeps delivery credentials out of retained job diagnostics.
package jobprivacy

import "strings"

const (
	FileFetch     = "FILE_FETCH"
	MediaGenerate = "MEDIA_GENERATE"
)

const privateMediaDiagnostic = "MEDIA_GENERATE input diagnostics omitted"

// IsPrivateMedia classifies before execution parsing. Any inputs field is
// private, including nil/malformed values and already sealed execution inputs.
// Legacy text jobs without inputs retain their existing diagnostic behavior.
func IsPrivateMedia(kind string, payload map[string]any) bool {
	_, present := payload["inputs"]
	return kind == MediaGenerate && present
}

// Payload is for retention/diagnostics only, never execution or redispatch.
// It drops FILE_FETCH and private-media payloads entirely and removes the node
// passcode field from every other job. Execution authority is never mutated.
func Payload(kind string, payload map[string]any) map[string]any {
	if kind == FileFetch || IsPrivateMedia(kind, payload) {
		return nil
	}
	return WithoutPasscode(payload)
}

// Text omits private-media diagnostics entirely. Other jobs retain the existing
// reflected delivery-credential/passcode redaction and safe-reason behavior.
func Text(kind string, payload map[string]any, text string) string {
	if IsPrivateMedia(kind, payload) {
		// Do not parse tokens or guess their escaping here. Before minting is
		// enabled, every private-media diagnostic must be static even if the
		// envelope/token is malformed or reflected through a quoted raw value.
		return privateMediaDiagnostic
	}
	if kind == FileFetch {
		if token, ok := payload["capability"].(string); ok && token != "" {
			text = replaceAll(text, token)
		}
	}
	return redactPasscode(payload, text)
}

func replaceAll(text, secret string) string {
	return strings.ReplaceAll(text, secret, "[REDACTED]")
}

type redactedError struct {
	// Keep the original error behind an explicit projection, not a reflectable
	// interface field. fmt can traverse private wrapper fields without calling
	// nested Error/Format methods. errors.Is/As still unwrap the original value.
	unwrap func() error
	text   string
}

// DeliveryError hides decoder-owned details before a job kind is known. It
// preserves explicit errors.Is/As classification without retaining a raw error
// value in an ordinary reflectable field or calling its diagnostic formatter.
func DeliveryError(err error) error {
	if err == nil {
		return nil
	}
	return &redactedError{unwrap: func() error { return err }, text: "job delivery decode failed"}
}

func (e *redactedError) Error() string { return e.text }

func (e *redactedError) Unwrap() error {
	if e.unwrap == nil {
		return nil
	}
	return e.unwrap()
}

// Error preserves classification/unwrapping and exposes only redacted text.
func Error(kind string, payload map[string]any, err error) error {
	if err == nil {
		return nil
	}
	if IsPrivateMedia(kind, payload) {
		return &redactedError{unwrap: func() error { return err }, text: privateMediaDiagnostic}
	}
	message := err.Error()
	if safe := Text(kind, payload, message); safe != message {
		return &redactedError{unwrap: func() error { return err }, text: safe}
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
	if kind != FileFetch && !IsPrivateMedia(kind, payload) {
		for key, value := range data {
			fields[key] = value
		}
	}
	return fields
}
