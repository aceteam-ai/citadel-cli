package platform

// In-process registry exclusion, not a process-ownership or authorization
// mechanism. The future suspend manager must also hold its action/lifecycle
// fences and the existing payload-store process lock. No public action uses
// the exclusive transaction entry point yet.

import (
	"context"
	"errors"
	"os"
	"sync"
)

const maxBrowserRegistryGates = 64

type browserRegistryGate struct {
	info  os.FileInfo
	token chan struct{}
	refs  int
}

var browserRegistryGates = struct {
	sync.Mutex
	entries map[*browserRegistryGate]struct{}
}{entries: make(map[*browserRegistryGate]struct{})}

func dropBrowserRegistryGate(g *browserRegistryGate) {
	browserRegistryGates.Lock()
	defer browserRegistryGates.Unlock()
	g.refs--
	if g.refs == 0 {
		delete(browserRegistryGates.entries, g)
	}
}

// Key by directory identity, not spelling: aliases must not evade exclusion.
// References include waiters, and disappear after the last release/cancellation.
// The registry gate must precede session and store/payload locks. Never acquire
// it while holding one of those locks, or re-enter a gated legacy writer.
func acquireBrowserRegistryGate(ctx context.Context, dir string) (func(), error) {
	fail := func() (func(), error) { return nil, errors.New("browser registry exclusion unavailable") }
	if ctx == nil || ctx.Err() != nil {
		return fail()
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return fail()
	}
	browserRegistryGates.Lock()
	var g *browserRegistryGate
	for entry := range browserRegistryGates.entries {
		if os.SameFile(info, entry.info) {
			g = entry
			break
		}
	}
	if g == nil {
		if len(browserRegistryGates.entries) >= maxBrowserRegistryGates {
			browserRegistryGates.Unlock()
			return fail()
		}
		g = &browserRegistryGate{info: info, token: make(chan struct{}, 1)}
		g.token <- struct{}{}
		browserRegistryGates.entries[g] = struct{}{}
	}
	g.refs++
	browserRegistryGates.Unlock()
	select {
	case <-ctx.Done():
		dropBrowserRegistryGate(g)
		return fail()
	case <-g.token:
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			g.token <- struct{}{}
			dropBrowserRegistryGate(g)
		})
	}
	named, err := os.Stat(dir)
	if ctx.Err() != nil || err != nil || !os.SameFile(info, named) {
		release()
		return fail()
	}
	return release, nil
}

// The trusted callback uses anchored store transitions, NEVER legacy save/
// delete helpers (the gate is deliberately non-reentrant). In particular it
// must not call ordinary teardown: it re-enters this non-reentrant gate and
// uses best-effort resource cleanup, not exact lifecycle completion.
// This entry only excludes in-process registry mutations, not live processes.
func (s *suspendIntentStore) withLifecycleExclusive(ctx context.Context, run func(context.Context) error) error {
	fail := func() error { return errors.New("browser lifecycle transaction incomplete") }
	if ctx == nil || run == nil || ctx.Err() != nil {
		return fail()
	}
	if _, finite := ctx.Deadline(); !finite {
		return fail()
	}
	release, err := acquireBrowserRegistryGate(ctx, s.root.Name())
	if err != nil {
		return fail()
	}
	defer release()
	check := func() bool {
		f, err := s.root.Open(".")
		if err != nil {
			return false
		}
		opened, e1 := f.Stat()
		_ = f.Close()
		named, e2 := os.Stat(s.root.Name())
		return ctx.Err() == nil && e1 == nil && e2 == nil && os.SameFile(opened, named) && s.private() == nil
	}
	if !check() || run(ctx) != nil || !check() {
		return fail()
	}
	return nil
}
