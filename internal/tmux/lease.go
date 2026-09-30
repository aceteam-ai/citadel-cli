package tmux

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// EnvSessionLeaseTTL configures the retention lease shared by terminal
// connections and TMUX_SESSION jobs.
const EnvSessionLeaseTTL = "CITADEL_TERMINAL_SESSION_TTL"

// MinimumSessionLeaseTTL is the shortest enabled retention lease. The terminal
// reaper and TMUX_SESSION job share this floor so neither path can create a
// lease the other path considers invalid.
const MinimumSessionLeaseTTL = time.Minute

// ErrInvalidSessionLeaseTTL identifies malformed, negative, or too-short
// persistent-session retention values.
var ErrInvalidSessionLeaseTTL = errors.New("persistent session TTL must be 0 (disabled) or at least 1 minute")

// ParseSessionLeaseTTL resolves one operator-provided retention value. Empty
// means unset and uses the seven-day default. The literal value 0 is the only
// disable sentinel; every other value must be a valid Go duration at or above
// MinimumSessionLeaseTTL.
func ParseSessionLeaseTTL(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return DefaultSessionLeaseTTL, nil
	}
	if value == "0" {
		return 0, nil
	}
	ttl, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%w: parse %q as a Go duration: %v", ErrInvalidSessionLeaseTTL, value, err)
	}
	if err := ValidateSessionLeaseTTL(ttl); err != nil {
		return 0, err
	}
	if ttl == 0 {
		return 0, fmt.Errorf("%w: use the literal value 0 to disable expiry", ErrInvalidSessionLeaseTTL)
	}
	return ttl, nil
}

// ValidateSessionLeaseTTL enforces the shared duration domain for callers that
// construct Config values directly rather than parsing EnvSessionLeaseTTL.
func ValidateSessionLeaseTTL(ttl time.Duration) error {
	if ttl < 0 || (ttl > 0 && ttl < MinimumSessionLeaseTTL) {
		return fmt.Errorf("%w: got %s", ErrInvalidSessionLeaseTTL, ttl)
	}
	return nil
}

// SessionLeaseTTLFromEnv parses and validates EnvSessionLeaseTTL.
func SessionLeaseTTLFromEnv() (time.Duration, error) {
	ttl, err := ParseSessionLeaseTTL(os.Getenv(EnvSessionLeaseTTL))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", EnvSessionLeaseTTL, err)
	}
	return ttl, nil
}

// SessionLeaseDeadline converts a TTL into the absolute timestamp stored in
// tmux. A non-positive TTL is represented by the zero time and stored as 0.
func SessionLeaseDeadline(now time.Time, ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return now.Add(ttl)
}
