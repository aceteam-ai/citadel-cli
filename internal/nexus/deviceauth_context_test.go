package nexus

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPollForTokenContextPendingThenApproved(t *testing.T) {
	mock := StartMockDeviceAuthServer(2)
	defer mock.Close()

	token, err := NewDeviceAuthClient(mock.URL()).PollForTokenContext(context.Background(), "mock-device-code-12345", 0)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if token.Authkey == "" || mock.GetPollCount() != 2 {
		t.Fatalf("want approval on the second poll, got token=%+v polls=%d", token, mock.GetPollCount())
	}
}

func TestPollForTokenContextTerminalOutcomes(t *testing.T) {
	for _, tc := range []struct {
		code string
		want error
	}{
		{"access_denied", ErrDeviceAuthDenied},
		{"expired_token", ErrDeviceAuthExpired},
	} {
		t.Run(tc.code, func(t *testing.T) {
			mock := StartMockDeviceAuthServer(1)
			defer mock.Close()
			mock.SetTerminalTokenError(tc.code)

			_, err := NewDeviceAuthClient(mock.URL()).PollForTokenContext(context.Background(), "mock-device-code-12345", 0)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			// The legacy wrapper keeps the text callers have always seen.
			_, legacyErr := NewDeviceAuthClient(mock.URL()).PollForToken("mock-device-code-12345", 0)
			if legacyErr == nil || legacyErr.Error() != tc.want.Error() {
				t.Fatalf("legacy wrapper text changed: %v", legacyErr)
			}
		})
	}
}

func TestPollForTokenContextCancelDuringWait(t *testing.T) {
	mock := StartMockDeviceAuthServer(1 << 30) // pending forever
	defer mock.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		// A 30s interval: only cancellation can end this poll in time.
		_, err := NewDeviceAuthClient(mock.URL()).PollForTokenContext(ctx, "mock-device-code-12345", 30)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for mock.GetPollCount() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("poll never reached the server")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not stop the poll promptly")
	}
}

func TestPollForTokenContextCancelDuringRequest(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := NewDeviceAuthClient(srv.URL).PollForTokenContext(ctx, "mock-device-code-12345", 1)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the server")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not abort the in-flight token request")
	}
}

func TestStartFlowContextCancelled(t *testing.T) {
	mock := StartMockDeviceAuthServer(1)
	defer mock.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewDeviceAuthClient(mock.URL()).StartFlowContext(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
