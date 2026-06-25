package reconcile

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// fakeOps is a STATEFUL in-memory ModuleOps for tests. Install/Update/Start/
// Stop/Uninstall mutate an internal store that the next ListInstalled reflects,
// so idempotency and update-detection tests are meaningful (a no-op stub would
// pass a fake idempotency check while the real diff was broken).
type fakeOps struct {
	mu      sync.Mutex
	store   map[string]*InstalledModule
	calls   []string          // ordered log of operations, e.g. "install:foo"
	failOn  map[string]error  // module name -> error to return from any op
	failOps map[string]string // module name -> specific op ("install","start",...) to fail; empty means all
}

func newFakeOps(seed ...InstalledModule) *fakeOps {
	f := &fakeOps{
		store:   map[string]*InstalledModule{},
		failOn:  map[string]error{},
		failOps: map[string]string{},
	}
	for _, im := range seed {
		cp := im
		f.store[im.Name] = &cp
	}
	return f
}

// failModule makes every op on name return err. If op is non-empty, only that
// op fails.
func (f *fakeOps) failModule(name, op string, err error) {
	f.failOn[name] = err
	f.failOps[name] = op
}

func (f *fakeOps) shouldFail(name, op string) error {
	err, ok := f.failOn[name]
	if !ok {
		return nil
	}
	if want := f.failOps[name]; want != "" && want != op {
		return nil
	}
	return err
}

func (f *fakeOps) Install(ctx context.Context, m ModuleAssignment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := m.Key()
	f.calls = append(f.calls, "install:"+name)
	if err := f.shouldFail(name, "install"); err != nil {
		return err
	}
	// Fresh install => running, with the assignment's source/config recorded.
	f.store[name] = &InstalledModule{
		Name:   name,
		Source: m.Source,
		Config: cloneConfig(m.Config),
		Health: HealthRunning,
	}
	return nil
}

func (f *fakeOps) Uninstall(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "uninstall:"+name)
	if err := f.shouldFail(name, "uninstall"); err != nil {
		return err
	}
	delete(f.store, name)
	return nil
}

func (f *fakeOps) Start(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "start:"+name)
	if err := f.shouldFail(name, "start"); err != nil {
		return err
	}
	if im, ok := f.store[name]; ok {
		im.Health = HealthRunning
	}
	return nil
}

func (f *fakeOps) Stop(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stop:"+name)
	if err := f.shouldFail(name, "stop"); err != nil {
		return err
	}
	if im, ok := f.store[name]; ok {
		im.Health = HealthStopped
	}
	return nil
}

func (f *fakeOps) ListInstalled(ctx context.Context) ([]InstalledModule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]InstalledModule, 0, len(f.store))
	for _, im := range f.store {
		cp := *im
		cp.Config = cloneConfig(im.Config)
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func cloneConfig(c map[string]string) map[string]string {
	if c == nil {
		return nil
	}
	out := make(map[string]string, len(c))
	for k, v := range c {
		out[k] = v
	}
	return out
}

func errf(format string, a ...any) error { return fmt.Errorf(format, a...) }
