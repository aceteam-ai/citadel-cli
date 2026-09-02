package heartbeat

import (
	"testing"
	"time"
)

// TestBackendHealth pins the 4-state classification (citadel-cli#429 Part 1)
// at its boundary times: DefaultStaleAfter (3min, success freshness) and
// attemptFreshWindow (90s, attempt freshness). Each case constructs a Marker
// directly (never via RecordSuccess/RecordFailure) and passes a fixed `now`,
// so this is hermetic and has no real-time dependency.
func TestBackendHealth(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		marker    *Marker
		wantState BackendState
		wantAge   time.Duration
	}{
		{
			name:      "nil marker is unknown",
			marker:    nil,
			wantState: BackendUnknown,
			wantAge:   0,
		},
		{
			name:      "zero-value marker (never written) is unknown",
			marker:    &Marker{},
			wantState: BackendUnknown,
			wantAge:   0,
		},
		{
			name: "fresh success, no failures is reachable",
			marker: &Marker{
				LastSuccessAt: now.Add(-12 * time.Second),
				LastAttemptAt: now.Add(-12 * time.Second),
			},
			wantState: BackendReachable,
			wantAge:   12 * time.Second,
		},
		{
			name: "fresh success, exactly one consecutive failure is still reachable",
			marker: &Marker{
				LastSuccessAt:       now.Add(-2 * time.Minute),
				LastAttemptAt:       now.Add(-10 * time.Second),
				ConsecutiveFailures: 1,
			},
			wantState: BackendReachable,
			wantAge:   2 * time.Minute,
		},
		{
			name: "success at exactly the DefaultStaleAfter boundary is still reachable",
			marker: &Marker{
				LastSuccessAt: now.Add(-DefaultStaleAfter),
				LastAttemptAt: now.Add(-DefaultStaleAfter),
			},
			wantState: BackendReachable,
			wantAge:   DefaultStaleAfter,
		},
		{
			name: "fresh success with two consecutive failures is degraded",
			marker: &Marker{
				LastSuccessAt:       now.Add(-1 * time.Minute),
				LastAttemptAt:       now.Add(-5 * time.Second),
				ConsecutiveFailures: 2,
			},
			wantState: BackendDegraded,
			wantAge:   1 * time.Minute,
		},
		{
			name: "fresh success with many consecutive failures is still degraded, not down",
			marker: &Marker{
				LastSuccessAt:       now.Add(-30 * time.Second),
				LastAttemptAt:       now.Add(-1 * time.Second),
				ConsecutiveFailures: 50,
			},
			wantState: BackendDegraded,
			wantAge:   30 * time.Second,
		},
		{
			name: "stale success but fresh attempt is down",
			marker: &Marker{
				LastSuccessAt:       now.Add(-10 * time.Minute),
				LastAttemptAt:       now.Add(-5 * time.Second),
				ConsecutiveFailures: 20,
				LastError:           "dial tcp: connection refused",
			},
			wantState: BackendDown,
			wantAge:   10 * time.Minute,
		},
		{
			name: "stale success, attempt at exactly the attemptFreshWindow boundary is down",
			marker: &Marker{
				LastSuccessAt: now.Add(-10 * time.Minute),
				LastAttemptAt: now.Add(-attemptFreshWindow),
			},
			wantState: BackendDown,
			wantAge:   10 * time.Minute,
		},
		{
			name: "never succeeded but attempting is down with zero age",
			marker: &Marker{
				LastAttemptAt:       now.Add(-1 * time.Second),
				ConsecutiveFailures: 3,
			},
			wantState: BackendDown,
			wantAge:   0,
		},
		{
			name: "stale success and stale attempt is unknown, not down",
			marker: &Marker{
				LastSuccessAt: now.Add(-1 * time.Hour),
				LastAttemptAt: now.Add(-1 * time.Hour),
			},
			wantState: BackendUnknown,
			wantAge:   1 * time.Hour,
		},
		{
			name: "stale success, attempt just past the attemptFreshWindow boundary is unknown",
			marker: &Marker{
				LastSuccessAt: now.Add(-10 * time.Minute),
				LastAttemptAt: now.Add(-attemptFreshWindow - time.Second),
			},
			wantState: BackendUnknown,
			wantAge:   10 * time.Minute,
		},
		{
			name: "never succeeded, no attempt at all is unknown",
			marker: &Marker{
				ConsecutiveFailures: 5,
			},
			wantState: BackendUnknown,
			wantAge:   0,
		},
		{
			name: "never succeeded, attempt just past the attemptFreshWindow boundary is unknown",
			marker: &Marker{
				LastAttemptAt: now.Add(-attemptFreshWindow - time.Second),
			},
			wantState: BackendUnknown,
			wantAge:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotState, gotAge := BackendHealth(tt.marker, now)
			if gotState != tt.wantState {
				t.Errorf("BackendHealth() state = %v, want %v", gotState, tt.wantState)
			}
			if gotAge != tt.wantAge {
				t.Errorf("BackendHealth() age = %v, want %v", gotAge, tt.wantAge)
			}
		})
	}
}
