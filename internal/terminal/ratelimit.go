// internal/terminal/ratelimit.go
package terminal

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimiter provides per-IP rate limiting for connection attempts
type RateLimiter struct {
	mu       sync.RWMutex
	limiters map[string]*rateLimiterEntry

	// rps is the rate limit in requests per second
	rps float64

	// burst is the maximum burst size
	burst int

	// cleanupInterval is how often to clean up expired entries
	cleanupInterval time.Duration

	// entryTTL is how long entries are kept after last use
	entryTTL time.Duration

	// stopCleanup signals the cleanup goroutine to stop
	stopCleanup chan struct{}
}

// rateLimiterEntry holds a rate limiter and its last access time
type rateLimiterEntry struct {
	limiter    *rate.Limiter
	lastAccess time.Time
}

// NewRateLimiter creates a new per-IP rate limiter
func NewRateLimiter(rps float64, burst int) *RateLimiter {
	rl := &RateLimiter{
		limiters:        make(map[string]*rateLimiterEntry),
		rps:             rps,
		burst:           burst,
		cleanupInterval: 5 * time.Minute,
		entryTTL:        10 * time.Minute,
		stopCleanup:     make(chan struct{}),
	}

	// Start the cleanup goroutine
	go rl.cleanupLoop()

	return rl
}

// Allow checks if a request from the given IP is allowed
func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	entry, ok := rl.limiters[ip]
	if !ok {
		// Create a new limiter for this IP
		entry = &rateLimiterEntry{
			limiter:    rate.NewLimiter(rate.Limit(rl.rps), rl.burst),
			lastAccess: time.Now(),
		}
		rl.limiters[ip] = entry
	} else {
		entry.lastAccess = time.Now()
	}

	return entry.limiter.Allow()
}

// Wait blocks until a request from the given IP is allowed or returns an error
func (rl *RateLimiter) Wait(ip string) error {
	rl.mu.Lock()
	entry, ok := rl.limiters[ip]
	if !ok {
		entry = &rateLimiterEntry{
			limiter:    rate.NewLimiter(rate.Limit(rl.rps), rl.burst),
			lastAccess: time.Now(),
		}
		rl.limiters[ip] = entry
	} else {
		entry.lastAccess = time.Now()
	}
	limiter := entry.limiter
	rl.mu.Unlock()

	return limiter.Wait(context.Background())
}

// Reserve reserves a token for the given IP and returns a Reservation
func (rl *RateLimiter) Reserve(ip string) *rate.Reservation {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	entry, ok := rl.limiters[ip]
	if !ok {
		entry = &rateLimiterEntry{
			limiter:    rate.NewLimiter(rate.Limit(rl.rps), rl.burst),
			lastAccess: time.Now(),
		}
		rl.limiters[ip] = entry
	} else {
		entry.lastAccess = time.Now()
	}

	return entry.limiter.Reserve()
}

// cleanupLoop periodically removes expired rate limiter entries
func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(rl.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			rl.cleanup()
		case <-rl.stopCleanup:
			return
		}
	}
}

// cleanup removes entries that haven't been accessed recently
func (rl *RateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	cutoff := time.Now().Add(-rl.entryTTL)
	for ip, entry := range rl.limiters {
		if entry.lastAccess.Before(cutoff) {
			delete(rl.limiters, ip)
		}
	}
}

// Stop stops the rate limiter's cleanup goroutine
func (rl *RateLimiter) Stop() {
	close(rl.stopCleanup)
}

// Count returns the number of tracked IPs
func (rl *RateLimiter) Count() int {
	rl.mu.RLock()
	defer rl.mu.RUnlock()
	return len(rl.limiters)
}

// Reset clears all rate limiter entries
func (rl *RateLimiter) Reset() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.limiters = make(map[string]*rateLimiterEntry)
}
