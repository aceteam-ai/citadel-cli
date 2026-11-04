package cdpdiscovery

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

type fakeTransport func(*http.Request) (*http.Response, error)

func (f fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDiscoverRefusesInvalidPortWithoutRequest(t *testing.T) {
	calls := 0
	c := &http.Client{Transport: fakeTransport(func(*http.Request) (*http.Response, error) {
		calls++
		t.Error("invalid port requested")
		return nil, nil
	})}
	for _, port := range []int{-1, 0, 65536} {
		if got, err := Discover(context.Background(), port, c); err == nil || got != (Target{}) {
			t.Fatal("invalid port accepted", port)
		}
	}
	if got, err := Discover(context.Background(), 9222, nil); err == nil || got != (Target{}) {
		t.Fatal("nil client accepted")
	}
	if calls != 0 {
		t.Fatal("invalid discovery reached transport")
	}
}
func TestDiscoverEnforcesRequestDeadlineWithoutMutatingClient(t *testing.T) {
	for _, parentBudget := range []time.Duration{10 * time.Millisecond, time.Minute} {
		t.Run(parentBudget.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				original := time.Hour
				c := &http.Client{Timeout: original, Transport: fakeTransport(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}
				ctx, cancel := context.WithTimeout(context.Background(), parentBudget)
				defer cancel()
				start := time.Now()
				got, err := Discover(ctx, 9222, c)
				if !errors.Is(err, context.DeadlineExceeded) || got != (Target{}) || time.Since(start) != min(parentBudget, 5*time.Second) || c.Timeout != original {
					t.Fatal("deadline lost or client mutated", got, err, time.Since(start))
				}
			})
		})
	}
}

func TestDiscoverPreservesEarlierClientTimeoutClassification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &http.Client{Timeout: 2 * time.Millisecond, Transport: fakeTransport(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}
		start := time.Now()
		got, err := Discover(context.Background(), 9222, client)
		if !errors.Is(err, context.DeadlineExceeded) || got != (Target{}) || time.Since(start) != 2*time.Millisecond {
			t.Fatal("earlier client timeout lost", got, err, time.Since(start))
		}
	})
}
