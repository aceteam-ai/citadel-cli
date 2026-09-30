package tmux

import (
	"errors"
	"testing"
	"time"
)

func TestSessionLeaseTTLFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "unset uses default", value: "", want: DefaultSessionLeaseTTL},
		{name: "operator override", value: "90m", want: 90 * time.Minute},
		{name: "minimum", value: "1m", want: time.Minute},
		{name: "literal zero disables", value: "0", want: 0},
		{name: "below floor", value: "30s", wantErr: true},
		{name: "negative", value: "-1m", wantErr: true},
		{name: "zero duration is not literal zero", value: "0s", wantErr: true},
		{name: "malformed", value: "invalid", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvSessionLeaseTTL, tc.value)
			got, err := SessionLeaseTTLFromEnv()
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidSessionLeaseTTL) {
					t.Fatalf("SessionLeaseTTLFromEnv() error = %v, want ErrInvalidSessionLeaseTTL", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("SessionLeaseTTLFromEnv() = (%v, %v), want (%v, nil)", got, err, tc.want)
			}
		})
	}

	t.Setenv(EnvSessionLeaseTTL, "0")
	ttl, err := SessionLeaseTTLFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got := SessionLeaseDeadline(time.Unix(100, 0), ttl); !got.IsZero() {
		t.Fatalf("disabled deadline = %v, want zero", got)
	}
}
