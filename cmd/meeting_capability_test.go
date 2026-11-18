package cmd

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/capabilities"
	"github.com/aceteam-ai/citadel-cli/internal/config"
)

func TestMeetingCapReconcilerFiresOnceOnTransition(t *testing.T) {
	var capable atomic.Bool
	var fired atomic.Int32
	var detects atomic.Int32
	r := newMeetingCapReconciler(
		func() bool { detects.Add(1); return capable.Load() },
		func(context.Context) bool { fired.Add(1); return true },
	)
	ctx := context.Background()

	r.Reconcile(ctx)
	r.Reconcile(ctx)
	if fired.Load() != 0 {
		t.Fatalf("fired before capable: %d", fired.Load())
	}
	capable.Store(true)
	r.Reconcile(ctx)
	r.Reconcile(ctx)
	r.Reconcile(ctx)
	if fired.Load() != 1 {
		t.Fatalf("fired = %d, want exactly 1", fired.Load())
	}
	if detects.Load() != 3 {
		t.Errorf("detect ran %d times, want 3 (stops probing once done)", detects.Load())
	}
}

func TestMeetingCapReconcilerSingleFlight(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var detects atomic.Int32
	r := newMeetingCapReconciler(
		func() bool { detects.Add(1); started <- struct{}{}; <-release; return false },
		func(context.Context) bool { return true },
	)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); r.Reconcile(context.Background()) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first probe never started")
	}
	// While the first probe is in flight, further ticks must not probe again.
	for i := 0; i < 3; i++ {
		r.Reconcile(context.Background())
	}
	close(release)
	wg.Wait()
	if detects.Load() != 1 {
		t.Errorf("detects = %d, want 1 in flight", detects.Load())
	}
}

func TestNodeDirHookHonoursNodeDirOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CITADEL_NODE_DIR", dir)
	if got := capabilities.NodeDirHook(); got != dir {
		t.Fatalf("NodeDirHook() = %q, want override %q", got, dir)
	}
}

func TestMeetingCapReconcilerRetriesWhenSubscribeFails(t *testing.T) {
	var attempts atomic.Int32
	var ok atomic.Bool
	r := newMeetingCapReconciler(
		func() bool { return true },
		func(context.Context) bool { attempts.Add(1); return ok.Load() },
	)
	ctx := context.Background()
	r.Reconcile(ctx)
	r.Reconcile(ctx)
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2 (failure must retry, not mark done)", attempts.Load())
	}
	ok.Store(true)
	r.Reconcile(ctx)
	r.Reconcile(ctx)
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3 (done after first success)", attempts.Load())
	}
}

func TestMeetingReconcileEligible(t *testing.T) {
	auto := &capabilities.NodeCapabilities{Tags: []string{"cpu:general"}}
	detected := &capabilities.NodeCapabilities{Tags: []string{"cpu:general", "meeting"}}
	cases := []struct {
		name     string
		role     config.WorkerRole
		caps     *capabilities.NodeCapabilities
		manifest bool
		want     bool
	}{
		{"auto-detected without meeting", config.WorkerRole("full"), auto, false, true},
		{"control role excluded", config.WorkerRoleControl, auto, false, false},
		{"manifest-declared excluded", config.WorkerRole("full"), auto, true, false},
		{"startup-detected meeting excluded", config.WorkerRole("full"), detected, false, false},
		{"nil caps excluded", config.WorkerRole("full"), nil, false, false},
	}
	for _, c := range cases {
		if got := meetingReconcileEligible(c.role, c.caps, c.manifest); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestWarnThrottlePrintsOncePerReason(t *testing.T) {
	var got []string
	w := &warnThrottle{out: func(m string) { got = append(got, m) }, quiet: time.Hour}
	t0 := time.Now()
	w.warn(t0, "org id unknown", "a")
	w.warn(t0.Add(30*time.Second), "org id unknown", "a")
	if len(got) != 1 {
		t.Fatalf("same reason printed %d times, want 1", len(got))
	}
	w.warn(t0.Add(time.Minute), "subscribe failed: x", "b")
	if len(got) != 2 {
		t.Fatalf("changed reason should print; got %d", len(got))
	}
	w.warn(t0.Add(2*time.Hour), "subscribe failed: x", "b")
	if len(got) != 3 {
		t.Fatalf("after quiet interval should print again; got %d", len(got))
	}
}
