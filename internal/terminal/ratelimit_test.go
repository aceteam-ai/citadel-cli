// internal/terminal/ratelimit_test.go
package terminal

import (
	"testing"
	"time"
)

func TestRateLimiterAllow(t *testing.T) {
	// Create a rate limiter with 10 RPS and burst of 5
	rl := NewRateLimiter(10, 5)
	defer rl.Stop()

	ip := "192.168.1.1"

	// First 5 requests should be allowed (burst)
	for i := 0; i < 5; i++ {
		if !rl.Allow(ip) {
			t.Errorf("request %d should be allowed (within burst)", i+1)
		}
	}

	// 6th request should be denied (burst exhausted)
	if rl.Allow(ip) {
		t.Error("6th request should be denied (burst exhausted)")
	}
}

func TestRateLimiterMultipleIPs(t *testing.T) {
	rl := NewRateLimiter(10, 3)
	defer rl.Stop()

	ip1 := "192.168.1.1"
	ip2 := "192.168.1.2"

	// Use up burst for IP1
	for i := 0; i < 3; i++ {
		rl.Allow(ip1)
	}

	// IP2 should still have its full burst
	for i := 0; i < 3; i++ {
		if !rl.Allow(ip2) {
			t.Errorf("IP2 request %d should be allowed", i+1)
		}
	}

	// IP1 should be rate limited
	if rl.Allow(ip1) {
		t.Error("IP1 should be rate limited")
	}
}

func TestRateLimiterCount(t *testing.T) {
	rl := NewRateLimiter(10, 5)
	defer rl.Stop()

	if rl.Count() != 0 {
		t.Errorf("expected initial count 0, got %d", rl.Count())
	}

	rl.Allow("ip1")
	rl.Allow("ip2")
	rl.Allow("ip3")

	if rl.Count() != 3 {
		t.Errorf("expected count 3, got %d", rl.Count())
	}
}

func TestRateLimiterReset(t *testing.T) {
	rl := NewRateLimiter(10, 5)
	defer rl.Stop()

	rl.Allow("ip1")
	rl.Allow("ip2")

	rl.Reset()

	if rl.Count() != 0 {
		t.Errorf("expected count 0 after reset, got %d", rl.Count())
	}
}

func TestRateLimiterReserve(t *testing.T) {
	rl := NewRateLimiter(10, 5)
	defer rl.Stop()

	ip := "192.168.1.1"

	// Make a reservation
	r := rl.Reserve(ip)
	if r == nil {
		t.Fatal("expected reservation, got nil")
	}

	// Reservation should be OK (first one in burst)
	if !r.OK() {
		t.Error("reservation should be OK")
	}
}

func TestRateLimiterRefill(t *testing.T) {
	// Create a limiter with high RPS for quick refill
	rl := NewRateLimiter(100, 1)
	defer rl.Stop()

	ip := "192.168.1.1"

	// Use the one burst token
	if !rl.Allow(ip) {
		t.Error("first request should be allowed")
	}

	// Second request should fail
	if rl.Allow(ip) {
		t.Error("second request should fail (burst exhausted)")
	}

	// Wait for refill (at 100 RPS, one token every 10ms)
	time.Sleep(20 * time.Millisecond)

	// Should be allowed again after refill
	if !rl.Allow(ip) {
		t.Error("request should be allowed after refill")
	}
}

func TestRateLimiterStop(t *testing.T) {
	rl := NewRateLimiter(10, 5)

	// Stop should not panic
	rl.Stop()

	// Allow should still work after stop (just no cleanup)
	if !rl.Allow("ip1") {
		t.Error("Allow should still work after Stop")
	}
}
