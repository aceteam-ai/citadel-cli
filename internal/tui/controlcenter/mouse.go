package controlcenter

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// ResolveMouseEnabled computes the effective initial mouse state from the
// persisted preference and the --no-mouse flag. The flag is a session override:
// when set it forces mouse off regardless of the saved preference, but it does
// not mutate the persisted value. When the flag is unset, the persisted
// preference wins (defaulting to enabled when no config exists).
//
// Extracted as a pure function so the precedence is unit-testable without a
// terminal or a running app, and callable from cmd where the flag lives.
func ResolveMouseEnabled(flagNoMouse bool, savedEnabled bool) bool {
	if flagNoMouse {
		return false
	}
	return savedEnabled
}

// rect is a minimal, dependency-free rectangle used by the pane hit-test so the
// geometry logic is testable without constructing tview primitives.
type rect struct {
	x, y, w, h int
}

// contains reports whether the point (px, py) lies within the rectangle.
func (r rect) contains(px, py int) bool {
	return px >= r.x && px < r.x+r.w && py >= r.y && py < r.y+r.h
}

// paneAtPoint returns the pane constant whose rectangle contains the point, or
// -1 when the click landed outside every dashboard pane (e.g. the header, help
// bar, or a gap between panels). rects is indexed by the pane* constants.
//
// Pure function: the caller passes the current on-screen rectangles (from each
// primitive's GetRect) and the click coordinates, keeping tview out of the
// hit-test so it can be exercised directly in tests.
func paneAtPoint(rects [paneCount]rect, px, py int) int {
	for i := 0; i < paneCount; i++ {
		if rects[i].contains(px, py) {
			return i
		}
	}
	return -1
}

// paneRects snapshots the current on-screen rectangle of each dashboard pane.
// Only valid to call after the layout has been drawn at least once; GetRect
// returns zero-sized rects before the first draw, in which case paneAtPoint
// simply reports no hit (contains is false for zero width/height).
func (cc *ControlCenter) paneRects() [paneCount]rect {
	var rects [paneCount]rect
	set := func(pane int, p tview.Primitive) {
		if p == nil {
			return
		}
		x, y, w, h := p.GetRect()
		rects[pane] = rect{x: x, y: y, w: w, h: h}
	}
	set(paneNode, cc.nodePanel)
	set(paneSystem, cc.vitalsPanel)
	set(paneJobs, cc.jobsPanel)
	set(paneServices, cc.servicesView)
	set(paneActions, cc.actionsView)
	set(panePeers, cc.peersView)
	set(paneActivity, cc.activityView)
	return rects
}

// handleMouse is the Application-level mouse capture. It runs only for the
// dashboard page (the active page owns focus; other pages' primitives handle
// their own clicks natively). Its job is to keep the control center's OWN focus
// model (cc.focusedPane + the yellow border/title highlight) in sync with
// tview's native focus-on-click, and to map a double-click to the same "details"
// action that Enter triggers.
//
// It deliberately returns the event unchanged so tview still forwards it to the
// target primitive's MouseHandler — that is what gives us native row selection,
// input focus, and wheel scrolling for free. Keyboard handling is untouched
// (that path is SetInputCapture, a separate hook).
func (cc *ControlCenter) handleMouse(event *tcell.EventMouse, action tview.MouseAction) (*tcell.EventMouse, tview.MouseAction) {
	if event == nil {
		return event, action
	}

	// Only manage focus sync while the dashboard page is active and we are not in
	// a modal overlay. On other pages, or in a modal, pass the event straight
	// through to native handling.
	if cc.inModal || cc.pmgr == nil || !cc.pmgr.isDashboardActive() {
		return event, action
	}

	switch action {
	case tview.MouseLeftClick, tview.MouseLeftDown:
		x, y := event.Position()
		if pane := paneAtPoint(cc.paneRects(), x, y); pane >= 0 && pane != cc.focusedPane {
			cc.focusedPane = pane
			cc.updatePaneFocus()
		}
	case tview.MouseLeftDoubleClick:
		x, y := event.Position()
		if pane := paneAtPoint(cc.paneRects(), x, y); pane >= 0 {
			cc.focusedPane = pane
			cc.updatePaneFocus()
			// paneActions already runs its action on a single click (via each
			// cell's SetClickedFunc), so a double-click there must NOT re-trigger
			// it. For the other panes, a double-click maps to the same "details"
			// action that Enter triggers.
			if pane != paneActions {
				cc.handleEnter()
			}
		}
	}

	return event, action
}

// SetMouseEnabled toggles terminal mouse reporting on the running app with no
// restart. Persisting the choice is the caller's responsibility (the Settings
// pane). Safe to call before Run() sets up the app — it is a no-op until then.
func (cc *ControlCenter) SetMouseEnabled(enabled bool) {
	cc.mouseEnabled = enabled
	if cc.app != nil {
		cc.app.EnableMouse(enabled)
	}
}

// IsMouseEnabled reports the current resolved mouse state.
func (cc *ControlCenter) IsMouseEnabled() bool { return cc.mouseEnabled }
