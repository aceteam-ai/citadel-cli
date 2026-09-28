package cmd

import (
	"sync"
	"testing"
	"time"
)

// TestKeyedMutex pins that mutating APP_* ops serialize per short code while
// different short codes stay concurrent.
func TestKeyedMutex(t *testing.T) {
	km := &keyedMutex{locks: map[string]*sync.Mutex{}}

	// Same key: a second lock blocks until the first unlocks.
	unlock := km.lock("app-a")
	got := make(chan struct{})
	go func() {
		u := km.lock("app-a")
		u()
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("same-key lock was acquired while the first was still held")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("same-key lock did not proceed after the first unlocked")
	}

	// Different keys: never block each other.
	held := km.lock("app-x")
	defer held()
	done := make(chan struct{})
	go func() {
		u := km.lock("app-y")
		u()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("different-key locks blocked each other")
	}
}
