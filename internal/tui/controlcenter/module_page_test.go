package controlcenter

import (
	"testing"

	"github.com/rivo/tview"
)

// TestModulePageLoadSourcesSeedsList asserts the module page pre-populates its
// known-modules list from the ListSources hook, so the user picks from a seeded
// list instead of typing a source into a blank input.
func TestModulePageLoadSourcesSeedsList(t *testing.T) {
	rows := []ModuleSource{
		{Name: "vllm", Source: "vllm", Description: "GPU inference", Trusted: true},
		{Name: "WhatsApp", Description: "community bridge", Trusted: true, Special: "whatsapp"},
	}
	p := NewModulePage(ModuleInstallCallbacks{
		ListSources: func() []ModuleSource { return rows },
	})
	p.Build(tview.NewApplication())
	p.loadSources()

	if got := p.sourceList.GetItemCount(); got != 2 {
		t.Fatalf("sourceList has %d items, want 2", got)
	}
	primary, _ := p.sourceList.GetItemText(0)
	if primary != "✓ vllm" {
		t.Errorf("row 0 primary = %q, want %q (trusted check prefix)", primary, "✓ vllm")
	}
}

// TestModulePageLoadSourcesEmpty asserts a blank ListSources still renders a
// non-empty, non-crashing placeholder row rather than an empty list.
func TestModulePageLoadSourcesEmpty(t *testing.T) {
	p := NewModulePage(ModuleInstallCallbacks{
		ListSources: func() []ModuleSource { return nil },
	})
	p.Build(tview.NewApplication())
	p.loadSources()

	if got := p.sourceList.GetItemCount(); got != 1 {
		t.Fatalf("empty sources rendered %d items, want 1 placeholder", got)
	}
}

// TestModulePageChooseSpecialSwitchesPage asserts picking a special row (e.g.
// WhatsApp) invokes SelectSpecial with the page name instead of running the
// install form — this is how WhatsApp folds into the unified module list.
func TestModulePageChooseSpecialSwitchesPage(t *testing.T) {
	var switched string
	p := NewModulePage(ModuleInstallCallbacks{
		SelectSpecial: func(name string) { switched = name },
	})
	p.Build(tview.NewApplication())

	p.chooseSource(ModuleSource{Name: "WhatsApp", Special: "whatsapp"})

	if switched != "whatsapp" {
		t.Errorf("SelectSpecial called with %q, want %q", switched, "whatsapp")
	}
}

// TestModulePageChooseNormalSeedsInput asserts picking a normal row drops its
// source into the page state (which the source form input then displays).
func TestModulePageChooseNormalSeedsInput(t *testing.T) {
	p := NewModulePage(ModuleInstallCallbacks{})
	p.Build(tview.NewApplication())

	p.chooseSource(ModuleSource{Name: "vllm", Source: "vllm"})

	p.mu.Lock()
	src := p.source
	p.mu.Unlock()
	if src != "vllm" {
		t.Errorf("chooseSource seeded source = %q, want %q", src, "vllm")
	}
}

// TestNextFormFocus covers the actual reported nav bug: Tab must cycle between
// form fields/buttons and only hand off (atBoundary) at the true first/last
// element, so a mid-form Tab never accidentally leaves the module tab.
func TestNextFormFocus(t *testing.T) {
	cases := []struct {
		name         string
		cur, total   int
		forward      bool
		wantNext     int
		wantBoundary bool
	}{
		// A 3-element form (input + 2 buttons): Tab advances 0→1→2, boundary at 2.
		{"forward field to button", 0, 3, true, 1, false},
		{"forward button to button", 1, 3, true, 2, false},
		{"forward at last is boundary", 2, 3, true, 2, true},
		// Shift+Tab retreats 2→1→0, boundary at 0 (hand back to the list).
		{"backward button to button", 2, 3, false, 1, false},
		{"backward button to field", 1, 3, false, 0, false},
		{"backward at first is boundary", 0, 3, false, 0, true},
		// Degenerate: empty form is always a boundary in both directions.
		{"empty forward", 0, 0, true, 0, true},
		{"empty backward", 0, 0, false, 0, true},
		// Single element: any Tab is a boundary.
		{"single forward", 0, 1, true, 0, true},
		{"single backward", 0, 1, false, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next, boundary := nextFormFocus(tc.cur, tc.total, tc.forward)
			if next != tc.wantNext || boundary != tc.wantBoundary {
				t.Errorf("nextFormFocus(%d, %d, %v) = (%d, %v), want (%d, %v)",
					tc.cur, tc.total, tc.forward, next, boundary, tc.wantNext, tc.wantBoundary)
			}
		})
	}
}
