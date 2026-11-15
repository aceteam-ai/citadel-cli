package redisapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoRequest429IsTypedAndWindowClearsOn2xx(t *testing.T) {
	var limited atomic.Bool
	limited.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if limited.Load() {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"Rate limit exceeded"}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"messageId":"1-0"}`))
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	err := c.StreamAdd(context.Background(), "s", map[string]string{"a": "b"}, 10)
	rle, ok := AsRateLimitError(err)
	if !ok {
		t.Fatalf("want typed RateLimitError, got %v", err)
	}
	if rle.RetryAfter != 3*time.Second {
		t.Fatalf("Retry-After header not parsed: %s", rle.RetryAfter)
	}
	since, retry, open := c.RateLimitWindow()
	if !open || since.IsZero() || retry != 3*time.Second {
		t.Fatalf("window not recorded: %v %v %v", since, retry, open)
	}

	limited.Store(false)
	if err := c.StreamAdd(context.Background(), "s", map[string]string{"a": "b"}, 10); err != nil {
		t.Fatal(err)
	}
	if _, _, open := c.RateLimitWindow(); open {
		t.Fatal("window must clear on the next 2xx")
	}
}
