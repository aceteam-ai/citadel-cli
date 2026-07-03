package provisionedservice

import (
	"path/filepath"
	"sync"
	"testing"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	return New(filepath.Join(t.TempDir(), registryFileName))
}

// TestListEmptyWhenAbsent: a registry whose file has never been written lists
// empty without error.
func TestListEmptyWhenAbsent(t *testing.T) {
	r := newTestRegistry(t)
	entries, err := r.List()
	if err != nil {
		t.Fatalf("List on absent file: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("List = %v, want empty", entries)
	}
}

// TestRegisterListRoundTrip: an entry survives a round-trip through the file and
// an empty capability is defaulted.
func TestRegisterListRoundTrip(t *testing.T) {
	r := newTestRegistry(t)
	if err := r.Register(Entry{Name: "mod-a", Prefix: "mod-a", Port: 8137}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// A fresh Registry over the same file (separate process semantics).
	r2 := New(r.Path())
	entries, err := r2.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Name != "mod-a" || e.Prefix != "mod-a" || e.Port != 8137 {
		t.Fatalf("entry = %+v, want name/prefix mod-a port 8137", e)
	}
	if e.Capability != DefaultCapability {
		t.Fatalf("capability = %q, want default %q", e.Capability, DefaultCapability)
	}
}

// TestRegisterReplacesByName: re-registering the same name updates in place
// rather than duplicating.
func TestRegisterReplacesByName(t *testing.T) {
	r := newTestRegistry(t)
	if err := r.Register(Entry{Name: "mod", Prefix: "mod", Port: 1, Capability: "provision"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(Entry{Name: "mod", Prefix: "mod", Port: 2, Capability: "services"}); err != nil {
		t.Fatal(err)
	}
	entries, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1 (replace, not append)", len(entries))
	}
	if entries[0].Port != 2 || entries[0].Capability != "services" {
		t.Fatalf("entry = %+v, want port 2 cap services", entries[0])
	}
}

// TestRemove: removing an entry drops it; removing an absent name is a no-op.
func TestRemove(t *testing.T) {
	r := newTestRegistry(t)
	_ = r.Register(Entry{Name: "a", Prefix: "a"})
	_ = r.Register(Entry{Name: "b", Prefix: "b"})
	if err := r.Remove("a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := r.Remove("does-not-exist"); err != nil {
		t.Fatalf("Remove absent = %v, want nil (idempotent)", err)
	}
	entries, _ := r.List()
	if len(entries) != 1 || entries[0].Name != "b" {
		t.Fatalf("after remove entries = %+v, want only b", entries)
	}
}

// TestCapabilityForPrefix: lookup returns the module's capability, defaults an
// empty stored capability, and reports not-found for an unknown prefix.
func TestCapabilityForPrefix(t *testing.T) {
	r := newTestRegistry(t)
	_ = r.Register(Entry{Name: "wa", Prefix: "whatsapp", Capability: "provision"})
	_ = r.Register(Entry{Name: "svc", Prefix: "svc"}) // empty -> default

	if cap, ok := r.CapabilityForPrefix("whatsapp"); !ok || cap != "provision" {
		t.Fatalf("whatsapp -> (%q,%v), want (provision,true)", cap, ok)
	}
	if cap, ok := r.CapabilityForPrefix("svc"); !ok || cap != DefaultCapability {
		t.Fatalf("svc -> (%q,%v), want (%q,true)", cap, ok, DefaultCapability)
	}
	if cap, ok := r.CapabilityForPrefix("nope"); ok || cap != "" {
		t.Fatalf("nope -> (%q,%v), want (\"\",false)", cap, ok)
	}
}

// TestRegisterRejectsEmpty: name and prefix are required.
func TestRegisterRejectsEmpty(t *testing.T) {
	r := newTestRegistry(t)
	if err := r.Register(Entry{Prefix: "p"}); err == nil {
		t.Error("expected error for empty name")
	}
	if err := r.Register(Entry{Name: "n"}); err == nil {
		t.Error("expected error for empty prefix")
	}
}

// TestConcurrentRegister exercises the mutex: many goroutines register distinct
// names concurrently and all survive. Run with -race to catch data races.
func TestConcurrentRegister(t *testing.T) {
	r := newTestRegistry(t)
	const n = 20
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := string(rune('a'+i%26)) + "-" + string(rune('0'+i/26))
			if err := r.Register(Entry{Name: name, Prefix: name, Port: 8000 + i}); err != nil {
				t.Errorf("concurrent Register: %v", err)
			}
		}(i)
	}
	wg.Wait()
	entries, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != n {
		t.Fatalf("len = %d, want %d (no lost updates)", len(entries), n)
	}
}
