package services

import (
	"errors"
	"strings"
	"time"
)

const EnvOllamaKeepAlive = "OLLAMA_KEEP_ALIVE"

// OllamaKeepAliveComposeEnv supplies exactly one authoritative keep-alive
// value for Ollama. Unset or invalid best-effort policy supplies explicit empty
// to suppress ambient and .env fallback while retaining the engine default.
// Strict callers must refuse the returned error before engine-side effects.
// Other services are unchanged. The input environment is never mutated.
func OllamaKeepAliveComposeEnv(env []string, service string, keepAlive *string) ([]string, error) {
	if service != "ollama" {
		return env, nil
	}
	value := ""
	var policyErr error
	if keepAlive != nil {
		candidate := strings.TrimSpace(*keepAlive)
		if candidate == "-1" {
			value = candidate
		} else if duration, err := time.ParseDuration(candidate); err == nil && duration >= 0 {
			value = candidate
		} else {
			policyErr = errors.New("invalid ollama_keep_alive: use a non-negative duration or -1")
		}
	}
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key != EnvOllamaKeepAlive {
			result = append(result, entry)
		}
	}
	return append(result, EnvOllamaKeepAlive+"="+value), policyErr
}
