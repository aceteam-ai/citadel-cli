package tmux

import (
	"os"
	"time"
)

// EnvSessionLeaseTTL configures the retention lease shared by terminal
// connections and TMUX_SESSION jobs.
const EnvSessionLeaseTTL = "CITADEL_TERMINAL_SESSION_TTL"

// SessionLeaseTTLFromEnv resolves EnvSessionLeaseTTL with the same forgiving
// startup semantics used by the terminal service: unset or malformed values use
// the seven-day default, while zero explicitly disables expiry.
func SessionLeaseTTLFromEnv() time.Duration {
	value := os.Getenv(EnvSessionLeaseTTL)
	if value == "" {
		return DefaultSessionLeaseTTL
	}
	ttl, err := time.ParseDuration(value)
	if err != nil {
		return DefaultSessionLeaseTTL
	}
	return ttl
}

// SessionLeaseDeadline converts a TTL into the absolute timestamp stored in
// tmux. A non-positive TTL is represented by the zero time and stored as 0.
func SessionLeaseDeadline(now time.Time, ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return now.Add(ttl)
}
