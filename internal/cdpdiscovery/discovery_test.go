package cdpdiscovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
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

func TestListReturnsEveryPageWithPinnedDebuggerURL(t *testing.T) {
	body := `[
	 {"id":"a","type":"page","title":"App","url":"http://app.test/","webSocketDebuggerUrl":"ws://elsewhere:1/devtools/page/a"},
	 {"id":"w","type":"service_worker","url":"http://app.test/sw.js","webSocketDebuggerUrl":"ws://127.0.0.1:1/devtools/page/w"},
	 {"id":"p","type":"page","title":"Login","url":"http://login.test/","webSocketDebuggerUrl":"ws://127.0.0.1:1/devtools/page/p"}]`
	c := &http.Client{Transport: fakeTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	got, err := List(context.Background(), 4321, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "p" || got[1].Title != "Login" {
		t.Fatalf("targets = %+v", got)
	}
	for _, tg := range got {
		if !strings.HasPrefix(tg.WebSocketDebuggerURL, "ws://127.0.0.1:4321/") {
			t.Fatalf("debugger URL not pinned: %s", tg.WebSocketDebuggerURL)
		}
	}
	first, err := Discover(context.Background(), 4321, c)
	if err != nil || first.ID != "a" {
		t.Fatalf("Discover = %+v %v", first, err)
	}
}

func TestListBoundedKeepsWorkingPastThePageCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < MaxPageTargets+5; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":"p%d","type":"page","url":"http://app.test/","webSocketDebuggerUrl":"ws://127.0.0.1:1/devtools/page/p%d"}`, i, i)
	}
	b.WriteString("]")
	c := &http.Client{Transport: fakeTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(b.String())), Header: http.Header{}}, nil
	})}
	if _, err := List(context.Background(), 4321, c); err == nil {
		t.Fatal("List accepted more than the page cap")
	}
	got, overflow, err := ListBounded(context.Background(), 4321, c)
	if err != nil || !overflow || len(got) != MaxPageTargets {
		t.Fatalf("ListBounded: %d targets overflow=%v err=%v", len(got), overflow, err)
	}
}
