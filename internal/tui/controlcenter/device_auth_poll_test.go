package controlcenter

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func TestDeviceAuthorizationPollBackoffPersistsAcrossPending(t *testing.T) {
	at := time.Unix(1000, 0)
	expiresAt := at.Add(time.Hour)
	var waits []time.Duration
	var intervals []int
	answers := []error{
		&nexus.TokenError{ErrorCode: "slow_down", Interval: 8},
		fmt.Errorf("wrapped: %w", &nexus.TokenError{ErrorCode: "authorization_pending", Interval: 100}),
		&nexus.TokenError{ErrorCode: "slow_down"},
		nil,
	}
	key, err := pollDeviceAuthorization(context.Background(), &DeviceAuthConfig{DeviceCode: "fixture-grant", Interval: 5}, expiresAt,
		func(code string, interval int) (string, error) {
			if code != "fixture-grant" {
				t.Fatal("poll changed the grant")
			}
			intervals = append(intervals, interval)
			err := answers[0]
			answers = answers[1:]
			// Response time must not count toward the next required wait.
			at = at.Add(20 * time.Second)
			if err == nil {
				return "fixture-key", nil
			}
			return "", err
		}, func(_ context.Context, delay time.Duration) error {
			waits = append(waits, delay)
			at = at.Add(delay)
			return nil
		}, func() time.Time { return at })
	if err != nil || key != "fixture-key" {
		t.Fatal("poll did not finish the original fixture grant")
	}
	if !reflect.DeepEqual(waits, []time.Duration{5 * time.Second, 10 * time.Second, 10 * time.Second, 15 * time.Second}) ||
		!reflect.DeepEqual(intervals, []int{5, 10, 10, 15}) {
		t.Fatalf("waits=%v intervals=%v", waits, intervals)
	}
}

func TestDeviceAuthorizationRetryRequiresTypedProtocolError(t *testing.T) {
	for _, err := range []error{
		errors.New("authorization_pending"),
		&nexus.TokenError{ErrorCode: "access_denied", ErrorDescription: "authorization_pending"},
		&nexus.TokenError{ErrorCode: "expired_token"},
		&nexus.TokenError{ErrorCode: "unknown"},
	} {
		_, retry, got := nextDeviceAuthPollInterval(5*time.Second, err)
		if retry || got != err {
			t.Fatal("non-retry authority was treated as pending")
		}
	}
	var typedNil *nexus.TokenError
	if _, retry, err := nextDeviceAuthPollInterval(5*time.Second, typedNil); retry || err == nil {
		t.Fatal("typed nil token answer must refuse")
	}
	interval, retry, err := nextDeviceAuthPollInterval(30*time.Second, &nexus.TokenError{ErrorCode: "slow_down", Interval: 60})
	if err != nil || !retry || interval != time.Minute {
		t.Fatalf("server slowdown hint = %v retry=%v", interval, retry)
	}
	interval, retry, err = nextDeviceAuthPollInterval(interval, &nexus.TokenError{ErrorCode: "slow_down"})
	if err != nil || !retry || interval != 65*time.Second {
		t.Fatal("slowdown was capped or reset instead of increasing")
	}
	for _, tc := range []struct {
		current time.Duration
		hint    int
	}{{maxDeviceAuthDuration, 0}, {5 * time.Second, -1}} {
		if _, retry, err := nextDeviceAuthPollInterval(tc.current, &nexus.TokenError{ErrorCode: "slow_down", Interval: tc.hint}); retry || err == nil {
			t.Fatal("invalid slowdown duration must not wrap or retry")
		}
	}
	if interval, err := deviceAuthPollInterval(0); err != nil || interval != 5*time.Second {
		t.Fatal("missing initial interval must retain the five-second floor")
	}
	maxInt := int(^uint(0) >> 1)
	if int64(maxInt) > int64(maxDeviceAuthDuration/time.Second) {
		if _, err := deviceAuthPollInterval(maxInt); err == nil {
			t.Fatal("overflowing initial seconds must refuse")
		}
	}
}

func TestDeviceAuthorizationPollStopsAtExpiryWithoutRequest(t *testing.T) {
	at := time.Unix(1000, 0)
	expiresAt := at.Add(2 * time.Second)
	waits := 0
	key, err := pollDeviceAuthorization(context.Background(), &DeviceAuthConfig{Interval: 5}, expiresAt,
		func(string, int) (string, error) { t.Fatal("expired grant was polled"); return "", nil },
		func(_ context.Context, delay time.Duration) error {
			waits++
			if delay != 2*time.Second {
				t.Fatalf("expiry wait = %v", delay)
			}
			at = at.Add(delay)
			return nil
		}, func() time.Time { return at })
	if key != "" || !errors.Is(err, errDeviceAuthExpired) || waits != 1 {
		t.Fatal("expiry did not end polling before another request")
	}
}

func TestDeviceAuthorizationPollRejectsCancelledOrExpiredResult(t *testing.T) {
	for _, phase := range []string{"before-wait", "during-wait", "during-poll", "expired-result"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			at := time.Unix(1000, 0)
			expiresAt := at.Add(time.Minute)
			if phase == "before-wait" {
				cancel()
			}
			polls := 0
			key, err := pollDeviceAuthorization(ctx, &DeviceAuthConfig{Interval: 5}, expiresAt,
				func(string, int) (string, error) {
					polls++
					if phase == "during-poll" {
						cancel()
					} else {
						at = expiresAt
					}
					return "late-fixture-key", nil
				}, func(_ context.Context, delay time.Duration) error {
					at = at.Add(delay)
					if phase == "during-wait" {
						cancel()
					}
					return nil
				}, func() time.Time { return at })
			want := context.Canceled
			if phase == "expired-result" {
				want = errDeviceAuthExpired
			}
			if key != "" || !errors.Is(err, want) {
				t.Fatal("late or cancelled registration key escaped the poll fence")
			}
			if (phase == "before-wait" || phase == "during-wait") && polls != 0 {
				t.Fatal("cancellation still issued a request")
			}
		})
	}
}

func TestDeviceAuthorizationConnectionChecksAfterPollAndUIWork(t *testing.T) {
	for _, phase := range []string{"current", "cancelled-after-poll", "expired-after-poll"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			at := time.Unix(1000, 0)
			expiresAt := at.Add(time.Minute)
			key, err := pollDeviceAuthorization(ctx, &DeviceAuthConfig{DeviceCode: "fixture-grant", Interval: 5}, expiresAt,
				func(string, int) (string, error) { return "fixture-key", nil },
				func(_ context.Context, delay time.Duration) error { at = at.Add(delay); return nil },
				func() time.Time { return at })
			if err != nil || key == "" {
				t.Fatal("fixture did not reach the post-poll UI boundary")
			}
			if phase == "cancelled-after-poll" {
				cancel()
			} else if phase == "expired-after-poll" {
				at = expiresAt
			}
			calls := 0
			err = connectCurrentDeviceAuthorization(ctx, expiresAt, func() time.Time { return at }, func(gotKey, code string) error {
				calls++
				if gotKey != "fixture-key" || code != "fixture-grant" {
					t.Fatal("connection changed the approved grant")
				}
				return nil
			}, key, "fixture-grant")
			switch phase {
			case "current":
				if err != nil || calls != 1 {
					t.Fatal("current grant did not connect exactly once")
				}
			case "cancelled-after-poll":
				if !errors.Is(err, context.Canceled) || calls != 0 {
					t.Fatal("connection entered after modal cancellation")
				}
			case "expired-after-poll":
				if !errors.Is(err, errDeviceAuthExpired) || calls != 0 {
					t.Fatal("connection entered after grant expiry")
				}
			}
		})
	}
}
