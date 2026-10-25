package controlcenter

import (
	"context"
	"errors"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

var errDeviceAuthExpired = errors.New("device authorization expired")

const maxDeviceAuthDuration = time.Duration(1<<63 - 1)

func deviceAuthPollInterval(seconds int) (time.Duration, error) {
	if seconds < 5 {
		return 5 * time.Second, nil
	}
	if int64(seconds) > int64(maxDeviceAuthDuration/time.Second) {
		return 0, errors.New("invalid device authorization interval")
	}
	return time.Duration(seconds) * time.Second, nil
}

func deviceAuthPollValid(ctx context.Context, now, expiresAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !now.Before(expiresAt) {
		return errDeviceAuthExpired
	}
	return nil
}

func deviceAuthDeadline(config *DeviceAuthConfig, now time.Time) (time.Time, error) {
	if config == nil || config.ExpiresIn <= 0 || int64(config.ExpiresIn) > int64(maxDeviceAuthDuration/time.Second) || config.ExpiresAt.IsZero() {
		return time.Time{}, errors.New("invalid device authorization expiry")
	}
	if config.ExpiresAt.After(now.Add(time.Duration(config.ExpiresIn) * time.Second)) {
		return time.Time{}, errors.New("invalid device authorization expiry")
	}
	if !now.Before(config.ExpiresAt) {
		return time.Time{}, errDeviceAuthExpired
	}
	return config.ExpiresAt, nil
}

// Stop is a lifecycle signal independent of the modal's Escape cancellation.
// The watcher wakes waits; Err also checks Stop synchronously so admission
// cannot race ahead merely because that watcher has not yet been scheduled.
type deviceAuthLifecycleContext struct {
	context.Context
	stopped <-chan struct{}
	cancel  context.CancelFunc
}

func (c *deviceAuthLifecycleContext) Err() error {
	select {
	case <-c.stopped:
		c.cancel()
	default:
	}
	return c.Context.Err()
}

func deviceAuthPollContext(stopped <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	lifecycle := &deviceAuthLifecycleContext{Context: ctx, stopped: stopped, cancel: cancel}
	if stopped == nil {
		cancel()
	}
	if lifecycle.Err() == nil {
		go func() {
			select {
			case <-stopped:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	return lifecycle, cancel
}

// Fence credential acceptance and connection entry after any intervening UI
// work. This does not abort an admitted Connect, undo its effects, or change its
// existing success continuation if cancellation arrives during that call.
func connectCurrentDeviceAuthorization(ctx context.Context, expiresAt time.Time, now func() time.Time,
	connect func(*nexus.TokenResponse, string) error, token *nexus.TokenResponse, code string) error {
	if now == nil {
		return errors.New("device authorization clock is unavailable")
	}
	if err := deviceAuthPollValid(ctx, now(), expiresAt); err != nil {
		return err
	}
	if token == nil || token.Authkey == "" || code == "" {
		return errors.New("current device authorization is missing")
	}
	if connect == nil {
		return errors.New("device enrollment connection is unavailable")
	}
	// TokenResponse contains scalar fields. Pass an owned value copy, never a
	// saved bearer, shared credential map, or callback retained by the poller.
	accepted := *token
	return connect(&accepted, code)
}

// Only typed protocol answers can continue a grant. Error descriptions are
// untrusted text, not retry authority. RFC 8628 increases every later interval
// by at least five seconds after each slow_down answer.
func nextDeviceAuthPollInterval(current time.Duration, err error) (time.Duration, bool, error) {
	var tokenErr *nexus.TokenError
	if !errors.As(err, &tokenErr) {
		return current, false, err
	}
	if tokenErr == nil {
		return current, false, errors.New("invalid device authorization response")
	}
	switch tokenErr.ErrorCode {
	case "authorization_pending":
		return current, true, nil
	case "slow_down":
		if current > maxDeviceAuthDuration-5*time.Second || tokenErr.Interval < 0 {
			return current, false, errors.New("invalid device authorization interval")
		}
		next := current + 5*time.Second
		hint, hintErr := deviceAuthPollInterval(tokenErr.Interval)
		if hintErr != nil {
			return current, false, hintErr
		}
		if hint > next {
			next = hint
		}
		return next, true, nil
	default:
		return current, false, err
	}
}

// pollDeviceAuthorization schedules from each completed response, so slow
// requests cannot leave queued ticker events that bypass a new backoff. The
// callback fetches only; a late result is discarded before any local credential
// acceptance. The exact token is later admitted by the separate Connect fence.
func pollDeviceAuthorization(ctx context.Context, config *DeviceAuthConfig, expiresAt time.Time,
	poll func(string, int) (*nexus.TokenResponse, error), wait func(context.Context, time.Duration) error,
	now func() time.Time) (*nexus.TokenResponse, error) {
	if config == nil || poll == nil || wait == nil || now == nil {
		return nil, errors.New("device authorization polling is unavailable")
	}
	interval, err := deviceAuthPollInterval(config.Interval)
	if err != nil {
		return nil, err
	}
	for {
		at := now()
		if err := deviceAuthPollValid(ctx, at, expiresAt); err != nil {
			return nil, err
		}
		delay := interval
		if remaining := expiresAt.Sub(at); remaining < delay {
			delay = remaining
		}
		if err := wait(ctx, delay); err != nil {
			return nil, err
		}
		if err := deviceAuthPollValid(ctx, now(), expiresAt); err != nil {
			return nil, err
		}
		token, pollErr := poll(config.DeviceCode, int(interval/time.Second))
		if err := deviceAuthPollValid(ctx, now(), expiresAt); err != nil {
			return nil, err
		}
		if pollErr != nil {
			next, retry, err := nextDeviceAuthPollInterval(interval, pollErr)
			if !retry {
				return nil, err
			}
			interval = next
			continue
		}
		if token == nil || token.Authkey == "" {
			return nil, errors.New("device authorization returned no registration key")
		}
		fetched := *token
		return &fetched, nil
	}
}
