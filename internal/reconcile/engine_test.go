package reconcile

import (
	"context"
	"sort"
	"testing"
)

func ds(ms ...ModuleAssignment) DesiredState { return DesiredState{Modules: ms} }

// planActions extracts (action, name) pairs from a plan for compact assertions.
func planActions(p Plan) []string {
	out := make([]string, 0, len(p.Steps))
	for _, s := range p.Steps {
		out = append(out, string(s.Action)+":"+s.Name)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestReconcileDiff(t *testing.T) {
	tests := []struct {
		name    string
		desired DesiredState
		actual  []InstalledModule
		want    []string // sorted (action:name) pairs expected in the plan
	}{
		{
			name:    "install missing",
			desired: ds(ModuleAssignment{Name: "foo", Source: "foo", DesiredStatus: StatusRunning}),
			actual:  nil,
			want:    []string{"install:foo"},
		},
		{
			name:    "install missing, desired stopped -> install + stop",
			desired: ds(ModuleAssignment{Name: "foo", Source: "foo", DesiredStatus: StatusStopped}),
			actual:  nil,
			want:    []string{"install:foo", "stop:foo"},
		},
		{
			name:    "uninstall removed",
			desired: ds(),
			// STAMPED desired-state provenance: only a desired-state-managed entry
			// is drift-uninstall-eligible (citadel#624 D1). See the dedicated
			// TestReconcileDoesNotUninstallUnstampedModule for the protected case.
			actual: []InstalledModule{{Name: "foo", Source: "foo", Health: HealthRunning, ManagedBy: ManagedByDesiredState}},
			want:   []string{"uninstall:foo"},
		},
		{
			name:    "update on source-ref change",
			desired: ds(ModuleAssignment{Name: "foo", Source: "owner/repo@v2", DesiredStatus: StatusRunning}),
			actual:  []InstalledModule{{Name: "foo", Source: "owner/repo@v1", Health: HealthRunning}},
			want:    []string{"update:foo"},
		},
		{
			name: "update on config change",
			desired: ds(ModuleAssignment{Name: "foo", Source: "foo", Config: map[string]string{"PORT": "9090"},
				DesiredStatus: StatusRunning}),
			actual: []InstalledModule{{Name: "foo", Source: "foo", Config: map[string]string{"PORT": "8080"},
				Health: HealthRunning}},
			want: []string{"update:foo"},
		},
		{
			name:    "start transition (desired running, currently stopped)",
			desired: ds(ModuleAssignment{Name: "foo", Source: "foo", DesiredStatus: StatusRunning}),
			actual:  []InstalledModule{{Name: "foo", Source: "foo", Health: HealthStopped}},
			want:    []string{"start:foo"},
		},
		{
			name:    "stop transition (desired stopped, currently running)",
			desired: ds(ModuleAssignment{Name: "foo", Source: "foo", DesiredStatus: StatusStopped}),
			actual:  []InstalledModule{{Name: "foo", Source: "foo", Health: HealthRunning}},
			want:    []string{"stop:foo"},
		},
		{
			name:    "already converged -> empty plan",
			desired: ds(ModuleAssignment{Name: "foo", Source: "foo", DesiredStatus: StatusRunning}),
			actual:  []InstalledModule{{Name: "foo", Source: "foo", Health: HealthRunning}},
			want:    []string{},
		},
		{
			name:    "default desired status is running",
			desired: ds(ModuleAssignment{Name: "foo", Source: "foo"}),
			actual:  []InstalledModule{{Name: "foo", Source: "foo", Health: HealthStopped}},
			want:    []string{"start:foo"},
		},
		{
			name: "mixed: install one, uninstall one, update one, leave one",
			desired: ds(
				ModuleAssignment{Name: "new", Source: "new", DesiredStatus: StatusRunning},
				ModuleAssignment{Name: "drift", Source: "drift@v2", DesiredStatus: StatusRunning},
				ModuleAssignment{Name: "ok", Source: "ok", DesiredStatus: StatusRunning},
			),
			actual: []InstalledModule{
				{Name: "drift", Source: "drift@v1", Health: HealthRunning},
				{Name: "ok", Source: "ok", Health: HealthRunning},
				// "stale" is stamped, so it is drift-uninstall-eligible (#624 D1).
				{Name: "stale", Source: "stale", Health: HealthRunning, ManagedBy: ManagedByDesiredState},
			},
			want: []string{"uninstall:stale", "update:drift", "install:new"},
		},
		{
			name:    "source change AND stopped -> update + stop",
			desired: ds(ModuleAssignment{Name: "foo", Source: "foo@v2", DesiredStatus: StatusStopped}),
			actual:  []InstalledModule{{Name: "foo", Source: "foo@v1", Health: HealthRunning}},
			want:    []string{"update:foo", "stop:foo"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Reconcile(context.Background(), tt.desired, tt.actual)
			if err != nil {
				t.Fatalf("Reconcile error: %v", err)
			}
			got := planActions(plan)
			// For multi-step plans with no ordering dependency in the assertion,
			// compare as sorted sets EXCEPT where order is part of the contract
			// (uninstall-before-install-before-transition). We assert the exact
			// ordered sequence the engine guarantees.
			if !eq(got, tt.want) {
				// Fall back to set comparison to give a clearer failure when the
				// only difference is ordering within a group.
				gs, ws := append([]string(nil), got...), append([]string(nil), tt.want...)
				sort.Strings(gs)
				sort.Strings(ws)
				if !eq(gs, ws) {
					t.Fatalf("plan mismatch:\n got: %v\nwant: %v", got, tt.want)
				}
				t.Fatalf("plan ordering mismatch:\n got: %v\nwant: %v", got, tt.want)
			}
		})
	}
}

// TestReconcileDoesNotUninstallUnstampedModule is the direct #4273 blast-radius
// regression for citadel#624 D1 (provenance-scoped uninstall). A NON-empty
// desired set containing ONLY one module must NOT uninstall an installed sibling
// that carries no desired-state provenance stamp (an operator/catalog-installed
// module, an embedded engine, or the bespoke WhatsApp bridge) -- only the STAMPED
// module in the desired set is drift-uninstall-eligible. This fails (the sibling
// is uninstalled) if the provenance gate in engine.go is removed.
func TestReconcileDoesNotUninstallUnstampedModule(t *testing.T) {
	desired := ds(ModuleAssignment{Name: "whatsapp-bridge", Source: "whatsapp-bridge", DesiredStatus: StatusRunning})
	actual := []InstalledModule{
		// The desired module, stamped + already converged.
		{Name: "whatsapp-bridge", Source: "whatsapp-bridge", Health: HealthRunning, ManagedBy: ManagedByDesiredState},
		// An UNSTAMPED sibling (operator-installed) NOT in the desired set.
		{Name: "operator-mod", Source: "owner/operator-mod@v1", Health: HealthRunning},
		// A stamped sibling NOT in the desired set MUST still be uninstalled (a
		// genuinely desired-state-managed module the plane removed).
		{Name: "old-managed", Source: "old-managed", Health: HealthRunning, ManagedBy: ManagedByDesiredState},
	}

	plan, err := Reconcile(context.Background(), desired, actual)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, s := range plan.Steps {
		if s.Action == ActionUninstall && s.Name == "operator-mod" {
			t.Fatalf("unstamped sibling %q must NOT be uninstalled (citadel#624 D1 blast-radius guard)", s.Name)
		}
	}
	// Sanity: the stamped-but-undesired sibling IS still uninstalled, so the gate
	// scopes rather than disables drift-uninstall.
	sawOldManaged := false
	for _, s := range plan.Steps {
		if s.Action == ActionUninstall && s.Name == "old-managed" {
			sawOldManaged = true
		}
	}
	if !sawOldManaged {
		t.Fatalf("stamped, undesired module %q must still be uninstalled; plan=%v", "old-managed", planActions(plan))
	}
}

// TestReconcileContextCancelled verifies Reconcile respects a cancelled context.
func TestReconcileContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Reconcile(ctx, ds(ModuleAssignment{Name: "foo", Source: "foo"}), nil); err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

// TestKeyDerivation checks the name-from-source fallback used as the diff key.
func TestKeyDerivation(t *testing.T) {
	cases := map[string]string{
		"embedding":                           "embedding",
		"owner/repo@v1.2.0":                   "repo",
		"https://host/owner/repo.git@ref":     "repo",
		"git@github.com:owner/repo.git":       "repo",
		"https://github.com/aceteam-ai/x.git": "x",
	}
	for src, want := range cases {
		if got := NameFromSource(src); got != want {
			t.Errorf("NameFromSource(%q) = %q, want %q", src, got, want)
		}
	}
	// An assignment with no explicit Name keys off the source.
	m := ModuleAssignment{Source: "owner/repo@v1"}
	if m.Key() != "repo" {
		t.Errorf("Key() = %q, want repo", m.Key())
	}
	// An explicit Name wins (so a ref change is not read as remove+add).
	m2 := ModuleAssignment{Name: "fixed", Source: "owner/repo@v9"}
	if m2.Key() != "fixed" {
		t.Errorf("Key() = %q, want fixed", m2.Key())
	}
}
