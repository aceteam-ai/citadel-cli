package cmd

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/capabilities"
)

func TestMeetingCapReconcilerFiresOnceOnTransition(t *testing.T) {
	var capable atomic.Bool
	var fired atomic.Int32
	var detects atomic.Int32
	r := newMeetingCapReconciler(
		func() bool { detects.Add(1); return capable.Load() },
		func(context.Context) { fired.Add(1) },
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
		func(context.Context) {},
	)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); r.Reconcile(context.Background()) }()
	<-started
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
