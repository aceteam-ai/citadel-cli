package controlcenter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func TestDeviceAuthorizationDeadlineDoesNotRestartAtPresentation(t *testing.T) {
	requestEntry := time.Unix(1000, 0)
	original := requestEntry.Add(time.Minute)
	config := &DeviceAuthConfig{ExpiresIn: 60, ExpiresAt: original}
	for _, presentation := range []time.Time{requestEntry, requestEntry.Add(20 * time.Second), original.Add(-time.Nanosecond)} {
		deadline, err := deviceAuthDeadline(config, presentation)
		if err != nil || !deadline.Equal(original) {
			t.Fatal("presentation delay extended or replaced the original grant deadline")
		}
	}
	if _, err := deviceAuthDeadline(config, original); !errors.Is(err, errDeviceAuthExpired) {
		t.Fatal("grant must be expired at the original deadline")
	}
	for _, invalid := range []*DeviceAuthConfig{
		nil,
		{ExpiresIn: 60},
		{ExpiresIn: 0, ExpiresAt: original},
		{ExpiresIn: -1, ExpiresAt: original},
		{ExpiresIn: 60, ExpiresAt: original.Add(time.Second)},
	} {
		if _, err := deviceAuthDeadline(invalid, requestEntry); err == nil {
			t.Fatal("missing or invalid original expiry must not create a new grant lifetime")
		}
	}
}

func TestDeviceAuthorizationPollAndAcceptanceCopyFullToken(t *testing.T) {
	at := time.Unix(1000, 0)
	expiresAt := at.Add(time.Minute)
	want := nexus.TokenResponse{
		Authkey: "fixture-key", DeviceAPIToken: "fixture-bearer", ExpiresIn: 999999,
		NexusURL: "https://control.invalid", APIBaseURL: "https://api.invalid/base",
		OrgID: "fixture-org", OrgName: "Fixture Org", UserEmail: "fixture@example.invalid",
		UserName: "Fixture User", FabricNodeID: "42", RedisURL: "redis://fixture.invalid",
		LeafPem: "fixture-leaf", ChainPem: "fixture-chain", NodeUID: "fixture-node",
	}
	fetched := want
	token, err := pollDeviceAuthorization(context.Background(), &DeviceAuthConfig{DeviceCode: "fixture-grant", Interval: 5}, expiresAt,
		func(string, int) (*nexus.TokenResponse, error) { return &fetched, nil },
		func(_ context.Context, delay time.Duration) error { at = at.Add(delay); return nil },
		func() time.Time { return at })
	if err != nil || token == nil || token == &fetched || *token != want {
		t.Fatal("poll must return an owned copy of the complete fetched token")
	}
	fetched.Authkey = "changed-provider-key"
	fetched.DeviceAPIToken = "changed-provider-bearer"
	calls := 0
	err = connectCurrentDeviceAuthorization(context.Background(), expiresAt, func() time.Time { return at }, func(accepted *nexus.TokenResponse, code string) error {
		calls++
		if accepted == token || *accepted != want || code != "fixture-grant" {
			t.Fatal("acceptance changed the grant, lost token fields, or retained a shared pointer")
		}
		accepted.Authkey = "changed-callback-key"
		return nil
	}, token, "fixture-grant")
	if err != nil || calls != 1 || *token != want {
		t.Fatal("acceptance must run once with its own copy and leave the fetched value unchanged")
	}
	// A long token expiry cannot renew the original modal authorization.
	at = expiresAt
	if err := connectCurrentDeviceAuthorization(context.Background(), expiresAt, func() time.Time { return at }, func(*nexus.TokenResponse, string) error {
		t.Fatal("token lifetime extended the grant's original deadline")
		return nil
	}, token, "fixture-grant"); !errors.Is(err, errDeviceAuthExpired) {
		t.Fatal("original grant deadline was not enforced after token fetch")
	}
}

func TestDeviceAuthorizationQueuedAcceptanceChecksCancelExpiryAndStopBeforeWrites(t *testing.T) {
	for _, phase := range []string{"current", "cancelled", "expired", "stopped"} {
		t.Run(phase, func(t *testing.T) {
			stopped := make(chan struct{})
			ctx, cancel := deviceAuthPollContext(stopped)
			defer cancel()
			at := time.Unix(1000, 0)
			expiresAt := at.Add(time.Minute)
			token, err := pollDeviceAuthorization(ctx, &DeviceAuthConfig{DeviceCode: "fixture-grant", Interval: 5}, expiresAt,
				func(string, int) (*nexus.TokenResponse, error) {
					return &nexus.TokenResponse{Authkey: "fixture-key", DeviceAPIToken: "fixture-bearer"}, nil
				}, func(_ context.Context, delay time.Duration) error { at = at.Add(delay); return nil },
				func() time.Time { return at })
			if err != nil || token == nil {
				t.Fatal("fixture did not fetch the approved current token")
			}
			path := filepath.Join(t.TempDir(), "credentials")
			if err := os.WriteFile(path, []byte("previous-credential-bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			writes := 0
			// Form queued work while the result is current, then delay its actual
			// invocation. No tview event loop or native UI is initialized.
			queued := func() error {
				return connectCurrentDeviceAuthorization(ctx, expiresAt, func() time.Time { return at }, func(accepted *nexus.TokenResponse, code string) error {
					if accepted.Authkey != "fixture-key" || code != "fixture-grant" {
						t.Fatal("admission substituted a saved credential or another grant")
					}
					writes++
					return os.WriteFile(path, []byte(accepted.DeviceAPIToken), 0600)
				}, token, "fixture-grant")
			}
			switch phase {
			case "cancelled":
				cancel()
			case "expired":
				at = expiresAt
			case "stopped":
				close(stopped)
			}
			err = queued()
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if phase == "current" {
				if err != nil || writes != 1 || string(data) != "fixture-bearer" {
					t.Fatal("healthy admission did not perform its real fake credential write exactly once")
				}
			} else {
				want := context.Canceled
				if phase == "expired" {
					want = errDeviceAuthExpired
				}
				if !errors.Is(err, want) || writes != 0 || string(data) != "previous-credential-bytes" {
					t.Fatal("delayed stale admission changed local credentials")
				}
			}
		})
	}
}

func TestDeviceAuthorizationStopDuringFetchDiscardsToken(t *testing.T) {
	stopped := make(chan struct{})
	ctx, cancel := deviceAuthPollContext(stopped)
	defer cancel()
	at := time.Unix(1000, 0)
	token, err := pollDeviceAuthorization(ctx, &DeviceAuthConfig{DeviceCode: "fixture-grant", Interval: 5}, at.Add(time.Minute),
		func(string, int) (*nexus.TokenResponse, error) {
			close(stopped)
			return &nexus.TokenResponse{Authkey: "late-key", DeviceAPIToken: "late-bearer"}, nil
		}, func(_ context.Context, delay time.Duration) error { at = at.Add(delay); return nil },
		func() time.Time { return at })
	if token != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("Stop during fetch allowed a late token to escape the post-fetch fence")
	}
}

func TestDeviceAuthorizationAdmissionObservesStopWithoutWatcher(t *testing.T) {
	stopped := make(chan struct{})
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Deliberately do not start the watcher: this models it being unscheduled
	// when Stop has already closed its signal at the acceptance boundary.
	ctx := &deviceAuthLifecycleContext{Context: base, stopped: stopped, cancel: cancel}
	close(stopped)
	if base.Err() != nil {
		t.Fatal("fixture cancelled before the synchronous Stop check")
	}
	calls := 0
	err := connectCurrentDeviceAuthorization(ctx, time.Unix(2000, 0), func() time.Time { return time.Unix(1000, 0) }, func(*nexus.TokenResponse, string) error {
		calls++
		return nil
	}, &nexus.TokenResponse{Authkey: "fixture-key"}, "fixture-grant")
	if !errors.Is(err, context.Canceled) || base.Err() != context.Canceled || calls != 0 {
		t.Fatal("admission relied on a scheduled watcher instead of synchronously observing Stop")
	}
	select {
	case <-base.Done():
	default:
		t.Fatal("Stop check reported an error without cancelling the real context")
	}
}

func TestDeviceAuthorizationPollRefusesMissingTokenOrRegistrationKey(t *testing.T) {
	for _, result := range []*nexus.TokenResponse{nil, {}, {DeviceAPIToken: "saved-bearer-alone"}} {
		at := time.Unix(1000, 0)
		calls := 0
		token, err := pollDeviceAuthorization(context.Background(), &DeviceAuthConfig{DeviceCode: "fixture-grant", Interval: 5}, at.Add(time.Minute),
			func(string, int) (*nexus.TokenResponse, error) {
				calls++
				return result, nil
			},
			func(_ context.Context, delay time.Duration) error { at = at.Add(delay); return nil },
			func() time.Time { return at })
		if err == nil || token != nil || calls != 1 {
			t.Fatal("missing current registration authority was accepted or silently retried")
		}
	}
}

func TestDeviceAuthorizationStoppedLifecycleRefusesWithoutPolling(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	for _, stopped := range []<-chan struct{}{nil, closed} {
		ctx, cancel := deviceAuthPollContext(stopped)
		if ctx.Err() != context.Canceled {
			cancel()
			t.Fatal("missing or already-stopped lifecycle did not cancel synchronously")
		}
		token, err := pollDeviceAuthorization(ctx, &DeviceAuthConfig{Interval: 5}, time.Unix(2000, 0),
			func(string, int) (*nexus.TokenResponse, error) { t.Fatal("stopped flow polled"); return nil, nil },
			func(context.Context, time.Duration) error { t.Fatal("stopped flow waited"); return nil },
			func() time.Time { return time.Unix(1000, 0) })
		cancel()
		if token != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("stopped flow did not refuse before its first request")
		}
	}
}

func TestDeviceAuthorizationLifecycleStopWakesWaiter(t *testing.T) {
	stopped := make(chan struct{})
	ctx, cancel := deviceAuthPollContext(stopped)
	defer cancel()
	close(stopped)
	select {
	case <-ctx.Done():
		if ctx.Err() != context.Canceled {
			t.Fatal("Stop did not cancel the real polling context")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not wake a polling wait")
	}
}

func TestDeviceAuthorizationAcceptanceRequiresCompleteCurrentInput(t *testing.T) {
	for _, input := range []struct {
		token *nexus.TokenResponse
		code  string
	}{
		{code: "fixture-grant"},
		{token: &nexus.TokenResponse{DeviceAPIToken: "saved-bearer-alone"}, code: "fixture-grant"},
		{token: &nexus.TokenResponse{Authkey: "fixture-key"}},
	} {
		err := connectCurrentDeviceAuthorization(context.Background(), time.Unix(2000, 0), func() time.Time { return time.Unix(1000, 0) },
			func(*nexus.TokenResponse, string) error {
				t.Fatal("incomplete current input entered acceptance")
				return nil
			}, input.token, input.code)
		if err == nil {
			t.Fatal("incomplete current input was accepted")
		}
	}
}

func TestDeviceAuthorizationAdmittedCallbackMayFinishAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err := connectCurrentDeviceAuthorization(ctx, time.Unix(2000, 0), func() time.Time { return time.Unix(1000, 0) }, func(*nexus.TokenResponse, string) error {
		calls++
		cancel()
		// Cancellation after entry cannot promise that an admitted synchronous
		// credential write, registration, or link was undone or revoked.
		return nil
	}, &nexus.TokenResponse{Authkey: "fixture-key"}, "fixture-grant")
	if err != nil || calls != 1 || ctx.Err() != context.Canceled {
		t.Fatal("admitted callback was incorrectly treated as an automatically undone operation")
	}
}
