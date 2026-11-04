package controlcenter

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func TestDeviceAuthorizationFlowCleanupFollowsLateFetchBeforeReturn(t *testing.T) {
	for _, phase := range []string{"cancelled", "expired", "stopped"} {
		t.Run(phase, func(t *testing.T) {
			const grant = "fixture-current-grant"
			const unrelated = "fixture-other-grant"
			stopped := make(chan struct{})
			ctx, cancel := deviceAuthPollContext(stopped)
			at := time.Unix(1000, 0)
			expiresAt := at.Add(time.Minute)
			config := &DeviceAuthConfig{DeviceCode: grant, Interval: 5}
			entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			var retryState sync.Map
			retryState.Store(grant, 1)
			retryState.Store(unrelated, 7)
			var cleanups atomic.Int32
			var wrongGrant atomic.Bool
			type result struct {
				token *nexus.TokenResponse
				err   error
			}
			returned := make(chan result, 1)
			callbacks := DeviceAuthCallbacks{
				PollToken: func(code string, _ int) (*nexus.TokenResponse, error) {
					if code != grant {
						wrongGrant.Store(true)
					}
					close(entered)
					<-release
					// Model the command poller storing a late 5xx retry count.
					// Cleanup before this return would permit stale recreation.
					retryState.Store(grant, 2)
					config.DeviceCode = unrelated
					return nil, &nexus.TokenError{ErrorCode: "authorization_pending"}
				},
				EndFlow: func(code string) {
					cleanups.Add(1)
					if code != grant {
						wrongGrant.Store(true)
					}
					retryState.Delete(code)
				},
			}
			go func() {
				defer close(finished)
				token, err := pollDeviceAuthorizationWithCleanup(ctx, config, expiresAt, callbacks,
					func(_ context.Context, delay time.Duration) error { at = at.Add(delay); return nil },
					func() time.Time { return at })
				returned <- result{token: token, err: err}
			}()
			t.Cleanup(func() {
				unblock()
				cancel()
				select {
				case <-finished:
				case <-time.After(2 * time.Second):
					t.Error("owned synchronous poll fixture did not join after release")
				}
			})
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("fixture never entered the synchronous token fetch")
			}
			switch phase {
			case "cancelled":
				cancel()
			case "expired":
				at = expiresAt
			case "stopped":
				close(stopped)
			}
			if count, ok := retryState.Load(grant); cleanups.Load() != 0 || !ok || count != 1 {
				t.Fatal("cleanup ran before the in-flight synchronous fetch returned")
			}
			select {
			case <-returned:
				t.Fatal("poll flow returned while its synchronous fetch was still held")
			default:
			}
			unblock()
			var got result
			select {
			case got = <-returned:
			case <-time.After(2 * time.Second):
				t.Fatal("late fetch did not finish its fenced flow")
			}
			want := context.Canceled
			if phase == "expired" {
				want = errDeviceAuthExpired
			}
			if got.token != nil || !errors.Is(got.err, want) || cleanups.Load() != 1 || wrongGrant.Load() {
				t.Fatal("late result escaped, cleanup was missing/repeated, or grant scope changed")
			}
			if _, ok := retryState.Load(grant); ok {
				t.Fatal("late fetch recreated retry state after cleanup")
			}
			if count, ok := retryState.Load(unrelated); !ok || count != 7 {
				t.Fatal("cleanup removed another authorization flow's state")
			}
		})
	}
}

func TestDeviceAuthorizationFlowCleanupRunsForSuccessFailureAndPreflight(t *testing.T) {
	for _, phase := range []string{"success", "terminal-error", "already-expired", "already-cancelled", "missing-poller"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			at := time.Unix(1000, 0)
			expiresAt := at.Add(time.Minute)
			if phase == "already-expired" {
				expiresAt = at
			} else if phase == "already-cancelled" {
				cancel()
			}
			var events []string
			refused := &nexus.TokenError{ErrorCode: "access_denied"}
			callbacks := DeviceAuthCallbacks{
				PollToken: func(string, int) (*nexus.TokenResponse, error) {
					events = append(events, "fetch")
					if phase == "terminal-error" {
						return nil, refused
					}
					return &nexus.TokenResponse{Authkey: "fixture-key"}, nil
				},
				EndFlow: func(code string) {
					if code != "fixture-grant" {
						t.Fatal("cleanup changed the flow's captured grant")
					}
					events = append(events, "cleanup")
				},
			}
			if phase == "missing-poller" {
				callbacks.PollToken = nil
			}
			token, err := pollDeviceAuthorizationWithCleanup(ctx, &DeviceAuthConfig{DeviceCode: "fixture-grant", Interval: 5}, expiresAt, callbacks,
				func(_ context.Context, delay time.Duration) error { at = at.Add(delay); return nil },
				func() time.Time { return at })
			events = append(events, "returned")
			wantEvents := []string{"cleanup", "returned"}
			if phase == "success" || phase == "terminal-error" {
				wantEvents = []string{"fetch", "cleanup", "returned"}
			}
			if !reflect.DeepEqual(events, wantEvents) {
				t.Fatalf("cleanup did not bracket the real polling flow: %v", events)
			}
			if phase == "success" {
				if err != nil || token == nil || token.Authkey != "fixture-key" {
					t.Fatal("healthy flow lost its token during cleanup")
				}
			} else if token != nil || err == nil {
				t.Fatal("failed flow fabricated an accepted token")
			}
			if phase == "terminal-error" && !errors.Is(err, refused) {
				t.Fatal("cleanup changed the terminal protocol error")
			}
		})
	}
}

func TestDeviceAuthorizationFlowAllowsNoCleanupCallback(t *testing.T) {
	at := time.Unix(1000, 0)
	token, err := pollDeviceAuthorizationWithCleanup(context.Background(), &DeviceAuthConfig{DeviceCode: "fixture-grant", Interval: 5}, at.Add(time.Minute),
		DeviceAuthCallbacks{PollToken: func(string, int) (*nexus.TokenResponse, error) {
			return &nexus.TokenResponse{Authkey: "fixture-key"}, nil
		}},
		func(_ context.Context, delay time.Duration) error { at = at.Add(delay); return nil },
		func() time.Time { return at })
	if err != nil || token == nil || token.Authkey != "fixture-key" {
		t.Fatal("optional cleanup callback became required for a healthy flow")
	}
}
