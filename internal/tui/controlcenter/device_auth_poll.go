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

// Fence actual connection entry, including UI work between polling and here.
// This does not abort an admitted Connect, undo its effects, or change its
// existing success continuation if cancellation arrives during that call.
func connectCurrentDeviceAuthorization(ctx context.Context, expiresAt time.Time, now func() time.Time,
	connect func(string, string) error, key, code string) error {
	if err := deviceAuthPollValid(ctx, now(), expiresAt); err != nil {
		return err
	}
	if connect == nil {
		return errors.New("device enrollment connection is unavailable")
	}
	return connect(key, code)
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
// callback still owns any network/persistence effects: rejecting a late result
// prevents subsequent Connect, but does not undo those effects or revoke a key.
func pollDeviceAuthorization(ctx context.Context, config *DeviceAuthConfig, expiresAt time.Time,
	poll func(string, int) (string, error), wait func(context.Context, time.Duration) error,
	now func() time.Time) (string, error) {
	if config == nil || poll == nil || wait == nil || now == nil {
		return "", errors.New("device authorization polling is unavailable")
	}
	interval, err := deviceAuthPollInterval(config.Interval)
	if err != nil {
		return "", err
	}
	for {
		at := now()
		if err := deviceAuthPollValid(ctx, at, expiresAt); err != nil {
			return "", err
		}
		delay := interval
		if remaining := expiresAt.Sub(at); remaining < delay {
			delay = remaining
		}
		if err := wait(ctx, delay); err != nil {
			return "", err
		}
		if err := deviceAuthPollValid(ctx, now(), expiresAt); err != nil {
			return "", err
		}
		key, pollErr := poll(config.DeviceCode, int(interval/time.Second))
		if err := deviceAuthPollValid(ctx, now(), expiresAt); err != nil {
			return "", err
		}
		if pollErr != nil {
			next, retry, err := nextDeviceAuthPollInterval(interval, pollErr)
			if !retry {
				return "", err
			}
			interval = next
			continue
		}
		if key == "" {
			return "", errors.New("device authorization returned no registration key")
		}
		return key, nil
	}
}
