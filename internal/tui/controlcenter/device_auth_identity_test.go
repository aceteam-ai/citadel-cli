package controlcenter

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func TestDeviceAuthGrantCallbacksSurvivePollCleanupAndStayCaptured(t *testing.T) {
	at := time.Unix(1000, 0)
	expires := at.Add(time.Minute)
	var order []string
	config := &DeviceAuthConfig{DeviceCode: "grant-a", Interval: 5, ExpiresIn: 60, ExpiresAt: expires,
		PollToken: func(code string, _ int) (*nexus.TokenResponse, error) {
			if code != "grant-a" {
				t.Fatal("grant callback rebound")
			}
			order = append(order, "poll")
			return &nexus.TokenResponse{Authkey: "fixture-authkey", LeafPem: "fixture-leaf"}, nil
		},
		Connect: func(token *nexus.TokenResponse, code string) error {
			if code != "grant-a" || token.LeafPem != "fixture-leaf" {
				t.Fatal("identity acceptance rebound")
			}
			order = append(order, "identity-accept")
			return nil
		},
	}
	captured := snapshotDeviceAuthConfig(config)
	wire, err := json.Marshal(captured)
	if err != nil || strings.Contains(string(wire), "PollToken") || strings.Contains(string(wire), "Connect") {
		t.Fatal("grant callbacks entered serialized presentation state")
	}
	config.DeviceCode = "grant-b"
	config.PollToken = func(string, int) (*nexus.TokenResponse, error) {
		t.Fatal("poll callback changed after capture")
		return nil, nil
	}
	config.Connect = func(*nexus.TokenResponse, string) error {
		t.Fatal("connect callback changed after capture")
		return nil
	}
	defaults := DeviceAuthCallbacks{
		PollToken: func(string, int) (*nexus.TokenResponse, error) {
			t.Fatal("CSR grant fell back to shared poll")
			return nil, nil
		},
		Connect: func(*nexus.TokenResponse, string) error {
			t.Fatal("CSR grant fell back to shared acceptance")
			return nil
		},
		EndFlow: func(code string) {
			if code != "grant-a" {
				t.Fatal("cleanup rebound")
			}
			order = append(order, "cleanup")
		},
	}
	token, err := pollDeviceAuthorizationWithCleanup(context.Background(), captured, expires, defaults, func(context.Context, time.Duration) error { return nil }, func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	if err := connectCurrentDeviceAuthorization(context.Background(), expires, func() time.Time { return at }, deviceAuthCallbacksFor(captured, defaults).Connect, token, captured.DeviceCode); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"poll", "cleanup", "identity-accept"}) {
		t.Fatal("poll cleanup discarded or prematurely accepted identity", order)
	}
}

func TestDeviceAuthGrantIdentityCannotBypassCancellationOrExpiry(t *testing.T) {
	for _, name := range []string{"cancelled", "expired", "stopped"} {
		t.Run(name, func(t *testing.T) {
			at := time.Unix(1000, 0)
			expiry := at.Add(time.Minute)
			writes := 0
			config := &DeviceAuthConfig{Connect: func(*nexus.TokenResponse, string) error { writes++; return nil }}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch name {
			case "cancelled":
				cancel()
			case "expired":
				at = expiry
			case "stopped":
				stopped := make(chan struct{})
				close(stopped)
				var release context.CancelFunc
				ctx, release = deviceAuthPollContext(stopped)
				defer release()
			}
			connect := deviceAuthCallbacksFor(config, DeviceAuthCallbacks{}).Connect
			err := connectCurrentDeviceAuthorization(ctx, expiry, func() time.Time { return at }, connect, &nexus.TokenResponse{Authkey: "fixture-authkey", LeafPem: "fixture-leaf"}, "grant")
			if err == nil || writes != 0 {
				t.Fatal("stale grant persisted an identity bundle")
			}
		})
	}
}

func TestDeviceAuthGrantWithoutIdentityKeepsSharedCallbacks(t *testing.T) {
	polls, connects := 0, 0
	defaults := DeviceAuthCallbacks{PollToken: func(string, int) (*nexus.TokenResponse, error) {
		polls++
		return &nexus.TokenResponse{Authkey: "fixture"}, nil
	}, Connect: func(*nexus.TokenResponse, string) error { connects++; return nil }}
	got := deviceAuthCallbacksFor(&DeviceAuthConfig{}, defaults)
	_, _ = got.PollToken("grant", 5)
	_ = got.Connect(&nexus.TokenResponse{Authkey: "fixture"}, "grant")
	if polls != 1 || connects != 1 {
		t.Fatal("legacy callbacks changed")
	}
	if snapshotDeviceAuthConfig(nil) != nil {
		t.Fatal("nil config fabricated a grant")
	}
}
