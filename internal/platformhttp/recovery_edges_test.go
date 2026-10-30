package platformhttp

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestFailureRotationCooldownIsSixtySeconds(t *testing.T) {
	// This is the approved policy value, independent of the tests that advance
	// their fake clock by the implementation constant.
	if FailureRotationCooldown != 60*time.Second {
		t.Fatal("failure rotation cooldown must stay at the approved sixty seconds")
	}
}

func TestMaximumAgeRotationPreservesOriginalFailureCooldown(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%v", h2), func(t *testing.T) {
			f := newSwapFixture(t, h2)
			// Age once inside the real sixty-second failure cooldown, without
			// aging again before its original deadline.
			f.transport.maxAge = 45 * time.Second
			f.failure.Store(503)
			for _, path := range []string{"/one", "/two", "/one", "/two"} {
				f.request(t, path)
			}
			if f.dials.Load() != 2 || strings.Count(f.diagnostic(), "re-dialing") != 1 {
				t.Fatal("initial failure rotation did not establish the fixture")
			}
			f.clock.Add(int64(45 * time.Second))
			for _, path := range []string{"/one", "/two", "/one", "/two"} {
				f.request(t, path)
			}
			if f.dials.Load() != 3 || strings.Count(f.diagnostic(), "re-dialing") != 2 || strings.Count(f.diagnostic(), "max_connection_age: re-dialing") != 1 {
				t.Fatal("age rotation erased the existing failure cooldown")
			}
			f.target.Store(f.next.Listener.Addr().String())
			f.clock.Add(int64(15*time.Second - time.Nanosecond))
			f.request(t, "/one")
			if f.dials.Load() != 3 || strings.Count(f.diagnostic(), "re-dialing") != 2 {
				t.Fatal("failure rotation happened before the original deadline")
			}
			f.clock.Add(int64(time.Nanosecond))
			f.request(t, "/two") // Retire the old pool without replaying.
			if status, body := f.request(t, "/recovered"); status != http.StatusOK || body != newBody || f.dials.Load() != 4 {
				t.Fatal("age rotation extended or erased the original cooldown")
			}
			if strings.Count(f.diagnostic(), "re-dialing") != 3 {
				t.Fatal("expected two failure rotations and one age rotation")
			}
		})
	}
}

func TestUnread404CloseContributesExactlyOnce(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%v", h2), func(t *testing.T) {
			f := newSwapFixture(t, h2)
			f.failure.Store(404)
			original := f.transport.current
			for i, path := range []string{"/one", "/two", "/one"} {
				resp, err := f.client.Get(f.baseURL + path)
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != http.StatusNotFound {
					resp.Body.Close()
					t.Fatal("fixture did not return a 404")
				}
				// Never call Read: Close must record the status without peeking
				// at the body, and a repeated Close must not count it again.
				// Exercise the actual http.Client body, including its timeout
				// wrapper, rather than asserting the transport's concrete type.
				firstClose, secondClose := resp.Body.Close(), resp.Body.Close()
				if firstClose != nil || secondClose != nil {
					t.Fatal("unread 404 closure changed caller behavior")
				}
				f.transport.mu.Lock()
				failed, current := original.failed, f.transport.current
				f.transport.mu.Unlock()
				if failed != i+1 || (current != original) != (i == 2) {
					t.Fatal("unread 404 was omitted, double counted or rotated early")
				}
			}
			if logs := f.diagnostic(); strings.Count(logs, "re-dialing") != 1 || !strings.Contains(logs, "http_404: re-dialing") {
				t.Fatal("unread status needs exactly one status-only recovery log")
			}
			f.target.Store(f.next.Listener.Addr().String())
			if status, body := f.request(t, "/recovered"); status != http.StatusOK || body != newBody {
				t.Fatal("unread 404 threshold did not reach the replacement server")
			}
		})
	}
}
