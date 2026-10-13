package clockstate

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClockStateCacheBoundaries(t *testing.T) {
	var ns atomic.Int64
	ns.Store(1000)
	m := &Monitor{now: func() time.Time { return time.Unix(0, ns.Load()) }, probe: func(context.Context) State { return Synced }}
	if got := m.Snapshot(); got != Unknown {
		t.Fatal("cold cache", got)
	}
	m.refresh(context.Background())
	ns.Add(int64(CacheTTL - time.Nanosecond))
	if got := m.Snapshot(); got != Synced {
		t.Fatal("fresh cache", got)
	}
	ns.Add(1)
	if got := m.Snapshot(); got != Unknown {
		t.Fatal("exact TTL must be stale", got)
	}
	ns.Store(999)
	if got := m.Snapshot(); got != Unknown {
		t.Fatal("negative fake age", got)
	}
	m.probe = func(context.Context) State { return State("spoof") }
	m.refresh(context.Background())
	if got := m.Snapshot(); got != Unknown {
		t.Fatal("invalid state", got)
	}
	m.probe = func(context.Context) State { return Unsynced }
	m.refresh(context.Background())
	if got := m.Snapshot(); got != Unsynced {
		t.Fatal("valid no", got)
	}
}

func TestClockStateBlockedProbeNeverBlocksGettersAndJoins(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	m := &Monitor{now: time.Now, probe: func(ctx context.Context) State {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		return Synced
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	t.Cleanup(cancel)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("fake probe did not enter")
	}
	// A second owner is refused; no second probing goroutine is launched.
	m.Run(ctx)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if m.Snapshot() != Unknown {
					t.Error("blocked probe reported synced")
				}
			}
		}()
	}
	getters := make(chan struct{})
	go func() { wg.Wait(); close(getters) }()
	select {
	case <-getters:
	case <-time.After(time.Second):
		t.Fatal("getters awaited fake command")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fake lifecycle did not join")
	}
	if calls.Load() != 1 || m.Snapshot() != Unknown {
		t.Fatal("cancelled probe published/overlapped")
	}
}

func TestClockStateRefreshIsOwnedAndNonoverlapping(t *testing.T) {
	ticks := make(chan time.Time)
	observed := make(chan struct{}, 2)
	var ns atomic.Int64
	m := &Monitor{now: func() time.Time { return time.Unix(0, ns.Load()) }, ticks: ticks, probe: func(context.Context) State { observed <- struct{}{}; return Synced }}
	if m.value.Load() != nil {
		t.Fatal("constructor probed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("initial fake refresh absent")
	}
	ns.Store(int64(CacheTTL))
	ticks <- time.Time{}
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("periodic fake refresh absent")
	}
}
