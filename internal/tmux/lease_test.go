package tmux

import (
	"testing"
	"time"
)

func TestSessionLeaseTTLFromEnv(t *testing.T) {
	t.Setenv(EnvSessionLeaseTTL, "90m")
	if got := SessionLeaseTTLFromEnv(); got != 90*time.Minute {
		t.Fatalf("TTL = %v, want 90m", got)
	}
	t.Setenv(EnvSessionLeaseTTL, "invalid")
	if got := SessionLeaseTTLFromEnv(); got != DefaultSessionLeaseTTL {
		t.Fatalf("invalid TTL = %v, want default %v", got, DefaultSessionLeaseTTL)
	}
	t.Setenv(EnvSessionLeaseTTL, "0")
	if got := SessionLeaseDeadline(time.Unix(100, 0), SessionLeaseTTLFromEnv()); !got.IsZero() {
		t.Fatalf("disabled deadline = %v, want zero", got)
	}
}
