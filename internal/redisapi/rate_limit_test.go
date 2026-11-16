package redisapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestConsume429KeepsTimingWithoutResponseText(t *testing.T) {
	const marker = "synthetic-consume-response-canary"
	for _, tc := range []struct {
		name, body string
		wait       time.Duration
	}{
		{"body hint", `{"error":"` + marker + `","window":"` + marker + `","retry_after":2}`, 2 * time.Second},
		{"header fallback", `{"error":"` + marker + `","window":"` + marker + `"}`, 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "3")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			var logs []string
			client := NewClient(ClientConfig{BaseURL: srv.URL, Token: "synthetic-token", DebugFunc: func(format string, args ...any) {
				logs = append(logs, fmt.Sprintf(format, args...))
			}})
			defer client.Close()
			_, err := client.ConsumeJob(context.Background(), ConsumeRequest{Queue: "q", Group: "g", Consumer: "c", BlockMs: 1, Count: 1})
			limited, ok := AsRateLimitError(err)
			if !ok || limited.StatusCode != http.StatusTooManyRequests || limited.Wait(time.Now()) != tc.wait {
				t.Fatalf("want typed 429 with %s timing, got %v", tc.wait, err)
			}
			diagnostics := append(logs, limited.Error(), limited.Message, limited.Body, limited.Window)
			for _, text := range diagnostics {
				if strings.Contains(text, marker) {
					t.Fatal("consume response text reached retained diagnostics")
				}
			}
		})
	}
}
