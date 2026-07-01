package controlcenter

import "testing"

func TestTabIndexFromRegion(t *testing.T) {
	cases := []struct {
		in      string
		wantIdx int
		wantOK  bool
	}{
		{"tab_0", 0, true},
		{"tab_5", 5, true},
		{"tab_12", 12, true},
		{"tab_", 0, false},
		{"tab_x", 0, false},
		{"nope", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			idx, ok := tabIndexFromRegion(tc.in)
			if ok != tc.wantOK || (ok && idx != tc.wantIdx) {
				t.Errorf("tabIndexFromRegion(%q) = (%d, %v), want (%d, %v)",
					tc.in, idx, ok, tc.wantIdx, tc.wantOK)
			}
		})
	}
}

func TestResolveMouseEnabled(t *testing.T) {
	cases := []struct {
		name        string
		flagNoMouse bool
		saved       bool
		want        bool
	}{
		{"flag off, saved on -> on", false, true, true},
		{"flag off, saved off -> off", false, false, false},
		{"flag on overrides saved on -> off", true, true, false},
		{"flag on, saved off -> off", true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveMouseEnabled(tc.flagNoMouse, tc.saved); got != tc.want {
				t.Errorf("ResolveMouseEnabled(%v, %v) = %v, want %v",
					tc.flagNoMouse, tc.saved, got, tc.want)
			}
		})
	}
}

func TestRectContains(t *testing.T) {
	r := rect{x: 10, y: 5, w: 20, h: 8} // covers x in [10,30), y in [5,13)

	cases := []struct {
		name   string
		px, py int
		want   bool
	}{
		{"top-left corner inside", 10, 5, true},
		{"interior", 20, 9, true},
		{"just left of edge", 9, 9, false},
		{"just right of edge (exclusive)", 30, 9, false},
		{"just above", 20, 4, false},
		{"just below (exclusive)", 20, 13, false},
		{"bottom-right inclusive corner", 29, 12, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.contains(tc.px, tc.py); got != tc.want {
				t.Errorf("contains(%d,%d) = %v, want %v", tc.px, tc.py, got, tc.want)
			}
		})
	}
}

func TestPaneAtPoint(t *testing.T) {
	// Lay out non-overlapping rects for a few panes; leave others zero-sized
	// (zero-width/height rects never match, simulating the pre-first-draw state).
	var rects [paneCount]rect
	rects[paneServices] = rect{x: 0, y: 0, w: 40, h: 10}
	rects[paneActions] = rect{x: 40, y: 0, w: 40, h: 10}
	rects[panePeers] = rect{x: 0, y: 10, w: 80, h: 10}

	cases := []struct {
		name   string
		px, py int
		want   int
	}{
		{"click in services", 5, 5, paneServices},
		{"click in actions", 45, 5, paneActions},
		{"click in peers", 20, 15, panePeers},
		{"click in a gap / unset pane", 60, 30, -1},
		{"click on a zero-sized pane's origin does not match", 0, 0, paneServices},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := paneAtPoint(rects, tc.px, tc.py); got != tc.want {
				t.Errorf("paneAtPoint(%d,%d) = %d, want %d", tc.px, tc.py, got, tc.want)
			}
		})
	}
}

// TestPaneAtPoint_ZeroRectsNoMatch verifies the pre-first-draw case: with every
// rect zero-sized (as GetRect returns before the layout is drawn), no click maps
// to a pane, so the mouse handler is a safe no-op instead of mis-focusing.
func TestPaneAtPoint_ZeroRectsNoMatch(t *testing.T) {
	var rects [paneCount]rect
	if got := paneAtPoint(rects, 0, 0); got != -1 {
		t.Errorf("paneAtPoint on all-zero rects = %d, want -1", got)
	}
	if got := paneAtPoint(rects, 100, 100); got != -1 {
		t.Errorf("paneAtPoint on all-zero rects = %d, want -1", got)
	}
}
