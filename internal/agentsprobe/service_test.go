package agentsprobe

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// writeFakeVendor drops an executable `claude` on a fake PATH dir that prints a
// version, so Probe reports Installed=true for it.
func writeFakeVendor(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake PATH exec uses POSIX shebang semantics")
	}
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'claude 1.2.3'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestProbe_HomeUnknownReportsUnknownNotConfidentNo is the #1015 pin: an
// unresolved target (HomeUnknown) must yield AuthStateUnknown for an installed
// vendor, NOT the confident AuthStateNo an empty-but-substituted /root would give.
func TestProbe_HomeUnknownReportsUnknownNotConfidentNo(t *testing.T) {
	pathDir := t.TempDir()
	writeFakeVendor(t, pathDir)

	ctx := context.Background()

	// HomeUnknown: target unresolved -> Unknown for the installed vendor.
	unknown := Probe(ctx, Options{PathEnv: pathDir, HomeUnknown: true})
	claudeU := findAgent(t, unknown, "claude")
	if !claudeU.Installed {
		t.Fatal("fake claude should be Installed on the fake PATH")
	}
	if claudeU.Authed != AuthStateUnknown {
		t.Fatalf("HomeUnknown probe: claude.Authed = %q, want %q", claudeU.Authed, AuthStateUnknown)
	}

	// Contrast: a resolved home with NO credential file gives the CONFIDENT
	// AuthStateNo -- which is exactly the false answer #1015 exists to avoid when
	// the home is actually /root and merely unknown.
	emptyHome := t.TempDir()
	confident := Probe(ctx, Options{PathEnv: pathDir, HomeDir: emptyHome})
	claudeC := findAgent(t, confident, "claude")
	if claudeC.Authed != AuthStateNo {
		t.Fatalf("resolved empty home: claude.Authed = %q, want %q (confident no)", claudeC.Authed, AuthStateNo)
	}
}

func findAgent(t *testing.T, agents []VendorAgent, name string) VendorAgent {
	t.Helper()
	for _, a := range agents {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("vendor %q not in probe result", name)
	return VendorAgent{}
}

// TestService_GetNeverProbes: the read path (what the endpoint default and any
// future heartbeat reader use) must not exec a probe.
func TestService_GetNeverProbes(t *testing.T) {
	var probes int32
	svc := NewService(ServiceConfig{
		Resolve: func() (Target, error) { return Target{Signal: SignalProcess}, nil },
		Probe: func(context.Context, Options) []VendorAgent {
			atomic.AddInt32(&probes, 1)
			return []VendorAgent{}
		},
	})
	for i := 0; i < 5; i++ {
		snap := svc.Get()
		if snap == nil || !snap.Stale {
			t.Fatalf("pre-probe Get should return a stale, non-nil snapshot; got %+v", snap)
		}
	}
	if n := atomic.LoadInt32(&probes); n != 0 {
		t.Fatalf("Get() probed %d times; must never probe", n)
	}
}

// TestService_RefreshSingleflightMinGap pins the ?refresh=1 contract: a forced
// probe runs, a second non-forced call within the min-gap returns the cache
// (no second exec), and a forced call always re-probes.
func TestService_RefreshSingleflightMinGap(t *testing.T) {
	var probes int32
	svc := NewService(ServiceConfig{
		MinRefreshGap: time.Hour, // large so "within gap" is deterministic
		Resolve: func() (Target, error) {
			return Target{Username: "alice", UID: 1000, HomeDir: "/home/alice", Signal: SignalNodeDirOwner}, nil
		},
		Probe: func(context.Context, Options) []VendorAgent {
			atomic.AddInt32(&probes, 1)
			return []VendorAgent{{Name: "claude", Installed: true}}
		},
	})
	ctx := context.Background()

	svc.Refresh(ctx, true) // startup-style forced probe
	if n := atomic.LoadInt32(&probes); n != 1 {
		t.Fatalf("after first forced Refresh: probes=%d, want 1", n)
	}
	// Non-forced within gap -> cache, no new probe.
	snap := svc.Refresh(ctx, false)
	if n := atomic.LoadInt32(&probes); n != 1 {
		t.Fatalf("non-forced Refresh within min-gap re-probed: probes=%d, want 1", n)
	}
	if snap.Target == nil || snap.Target.Signal != string(SignalNodeDirOwner) {
		t.Fatalf("snapshot target not attributed: %+v", snap.Target)
	}
	// Forced always re-probes.
	svc.Refresh(ctx, true)
	if n := atomic.LoadInt32(&probes); n != 2 {
		t.Fatalf("forced Refresh should re-probe: probes=%d, want 2", n)
	}
}

// TestService_RefreshConcurrentSingleflight: many concurrent non-forced callers
// arriving around the first probe collapse onto a single exec (singleflight).
func TestService_RefreshConcurrentSingleflight(t *testing.T) {
	var probes int32
	svc := NewService(ServiceConfig{
		MinRefreshGap: time.Hour,
		Resolve:       func() (Target, error) { return Target{Signal: SignalProcess}, nil },
		Probe: func(context.Context, Options) []VendorAgent {
			atomic.AddInt32(&probes, 1)
			time.Sleep(30 * time.Millisecond) // widen the race window
			return []VendorAgent{}
		},
	})
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); svc.Refresh(ctx, false) }()
	}
	wg.Wait()
	if n := atomic.LoadInt32(&probes); n != 1 {
		t.Fatalf("concurrent Refresh probed %d times; singleflight should collapse to 1", n)
	}
}

// TestService_UnresolvableTargetProbesHomeUnknown: a resolve error is recorded
// and the probe runs with HomeUnknown (the #1015 path), never against /root.
func TestService_UnresolvableTargetProbesHomeUnknown(t *testing.T) {
	var gotOpts Options
	svc := NewService(ServiceConfig{
		Resolve: func() (Target, error) { return Target{}, os.ErrNotExist },
		Probe: func(_ context.Context, opts Options) []VendorAgent {
			gotOpts = opts
			return []VendorAgent{}
		},
	})
	snap := svc.Refresh(context.Background(), true)
	if !gotOpts.HomeUnknown {
		t.Fatal("unresolvable target must probe with HomeUnknown")
	}
	if snap.ResolveError == "" {
		t.Fatal("unresolvable target must record a resolve_error")
	}
	if snap.Target != nil {
		t.Fatalf("unresolvable target must not fabricate a target block: %+v", snap.Target)
	}
}

// TestService_DisabledIsInertAndHonest: under the kill switch nothing probes and
// the snapshot is marked disabled.
func TestService_DisabledIsInertAndHonest(t *testing.T) {
	var probes int32
	svc := NewService(ServiceConfig{
		Disabled: true,
		Resolve:  func() (Target, error) { return Target{}, nil },
		Probe: func(context.Context, Options) []VendorAgent {
			atomic.AddInt32(&probes, 1)
			return nil
		},
	})
	svc.Start(context.Background(), time.Hour) // must be a no-op
	svc.Refresh(context.Background(), true)    // must be a no-op
	time.Sleep(20 * time.Millisecond)
	if n := atomic.LoadInt32(&probes); n != 0 {
		t.Fatalf("disabled service probed %d times", n)
	}
	if snap := svc.Get(); snap == nil || !snap.Disabled {
		t.Fatalf("disabled service snapshot should carry disabled=true; got %+v", snap)
	}
}

// TestNewService_SeedSnapshotIsEmptySlice: the pre-probe snapshot marshals its
// agents as [] (non-nil), not null.
func TestNewService_SeedSnapshotIsEmptySlice(t *testing.T) {
	svc := NewService(ServiceConfig{Resolve: func() (Target, error) { return Target{}, nil }})
	if svc.Get().Agents == nil {
		t.Fatal("seed snapshot Agents must be a non-nil empty slice, not nil")
	}
}
