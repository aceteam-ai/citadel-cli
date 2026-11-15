package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

// rateLimitedOnceSource fails its first poll with a typed 429 and records when
// each poll happened.
type rateLimitedOnceSource struct {
	*MockJobSource
	retryAfter time.Duration
	mu         sync.Mutex
	polls      []time.Time
}

func (s *rateLimitedOnceSource) Next(ctx context.Context) (*Job, error) {
	s.mu.Lock()
	s.polls = append(s.polls, time.Now())
	n := len(s.polls)
	s.mu.Unlock()
	if n == 1 {
		return nil, &redisapi.RateLimitError{StatusCode: 429, Message: "Rate limit exceeded", RetryAfter: s.retryAfter}
	}
	return nil, nil
}

func (s *rateLimitedOnceSource) pollTimes() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.polls...)
}

func TestRunnerConsumeWaitsRetryAfterOn429(t *testing.T) {
	// Retry-After 2s is above the 1s default backoff, so only honoring the
	// hint (not the exponential schedule) can produce a >=2s gap.
	src := &rateLimitedOnceSource{MockJobSource: NewMockJobSource("test", nil), retryAfter: 2 * time.Second}
	runner := NewRunner(src, nil, RunnerConfig{WorkerID: "w", MaxConcurrency: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { runner.Run(ctx); close(done) }()

	deadline := time.After(4 * time.Second)
	for len(src.pollTimes()) < 2 {
		select {
		case <-deadline:
			t.Fatal("consume never polled again after the 429")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-done

	polls := src.pollTimes()
	if gap := polls[1].Sub(polls[0]); gap < 2*time.Second {
		t.Fatalf("consume polled again after %s, want at least Retry-After (2s)", gap)
	}
}

func TestFetchBackoffClampsRetryAfter(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		err  error
		want time.Duration
		rl   bool
	}{
		{"floor", &redisapi.RateLimitError{RetryAfter: 10 * time.Millisecond}, time.Second, true},
		{"missing hint uses floor", &redisapi.RateLimitError{}, time.Second, true},
		{"within bounds", &redisapi.RateLimitError{RetryAfter: 7 * time.Second}, 7 * time.Second, true},
		{"cap", &redisapi.RateLimitError{RetryAfter: 24 * time.Hour}, 30 * time.Second, true},
		{"reset_at fallback", &redisapi.RateLimitError{ResetAt: now.Add(5 * time.Second)}, 5 * time.Second, true},
		{"other error keeps schedule", context.DeadlineExceeded, 4 * time.Second, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, rl := fetchBackoff(tc.err, 4*time.Second, now)
			if rl != tc.rl || (got != tc.want && !(tc.name == "reset_at fallback" && got > 4*time.Second && got <= 5*time.Second)) {
				t.Fatalf("fetchBackoff = %s,%v want %s,%v", got, rl, tc.want, tc.rl)
			}
		})
	}
}

func TestWorkerStateSurfacesRateLimitWindow(t *testing.T) {
	st := NewWorkerState()
	if snap := st.Snapshot(); snap.RateLimitedSince != nil || snap.LastRetryAfterSeconds != 0 {
		t.Fatal("rate-limit fields must be absent by default")
	}
	since := time.Now().Add(-5 * time.Second)
	open := true
	st.SetRateLimitSource(func() (time.Time, time.Duration, bool) { return since, 7 * time.Second, open })
	snap := st.Snapshot()
	if snap.RateLimitedSince == nil || !snap.RateLimitedSince.Equal(since) || snap.LastRetryAfterSeconds != 7 {
		t.Fatalf("window not surfaced: %+v", snap)
	}
	open = false
	if snap := st.Snapshot(); snap.RateLimitedSince != nil || snap.LastRetryAfterSeconds != 0 {
		t.Fatal("window must clear once requests succeed")
	}
}
