package jobs

import (
	"testing"
	"time"
)

// TestNewThrottledProgress_TimeThrottle pins the 2-second time throttle with an
// injected clock (no sleeping): the FIRST update always passes, intermediate
// updates inside the window are dropped, an update at/after 2s passes, and a
// Final update always passes. Total is 0 (unknown) so only the time rule applies.
func TestNewThrottledProgress_TimeThrottle(t *testing.T) {
	var now time.Time
	clock := func() time.Time { return now }
	var got []map[string]any
	emit := func(m map[string]any) { got = append(got, m) }
	p := NewThrottledProgress(emit, clock)

	now = time.Unix(0, 0)
	p(ProgressEvent{Stage: "index", Done: 0, Unit: "files"}) // first -> pass
	now = now.Add(500 * time.Millisecond)
	p(ProgressEvent{Stage: "index", Done: 1, Unit: "files"}) // within 2s -> drop
	now = now.Add(500 * time.Millisecond)
	p(ProgressEvent{Stage: "index", Done: 2, Unit: "files"}) // 1s -> drop
	now = now.Add(1100 * time.Millisecond)
	p(ProgressEvent{Stage: "index", Done: 3, Unit: "files"}) // 2.1s -> pass
	now = now.Add(100 * time.Millisecond)
	p(ProgressEvent{Stage: "index", Done: 4, Unit: "files", Final: true}) // final -> pass

	if len(got) != 3 {
		t.Fatalf("emitted %d updates, want 3 (first, >=2s, final): %v", len(got), got)
	}
	if got[0]["done"].(int) != 0 {
		t.Errorf("first emit done=%v, want 0", got[0]["done"])
	}
	if got[1]["done"].(int) != 3 {
		t.Errorf("second emit done=%v, want 3 (the 2.1s update)", got[1]["done"])
	}
	if got[2]["done"].(int) != 4 {
		t.Errorf("final emit done=%v, want 4", got[2]["done"])
	}
}

// TestNewThrottledProgress_PercentThrottle pins that a >=1% advance passes even
// inside the time window, and that the derived percent/rate/eta fields ride the
// event.
func TestNewThrottledProgress_PercentThrottle(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	var got []map[string]any
	p := NewThrottledProgress(func(m map[string]any) { got = append(got, m) }, clock)

	p(ProgressEvent{Stage: "index", Done: 0, Total: 100, Unit: "files"}) // first -> pass (percent 0)
	now = now.Add(10 * time.Millisecond)
	p(ProgressEvent{Stage: "index", Done: 1, Total: 100, Unit: "files"}) // percent 1 (+1) -> pass
	now = now.Add(10 * time.Millisecond)
	p(ProgressEvent{Stage: "index", Done: 1, Total: 100, Unit: "files"}) // same percent, <2s -> drop

	if len(got) != 2 {
		t.Fatalf("emitted %d updates, want 2 (percent threshold): %v", len(got), got)
	}
	if got[1]["percent"].(int) != 1 {
		t.Errorf("percent=%v, want 1", got[1]["percent"])
	}
	if _, ok := got[1]["rate"]; !ok {
		t.Error("expected a rate field on the emitted event")
	}
}

// TestNewThrottledProgress_NilSink is a no-op (a caller with no sink can wire it
// unconditionally).
func TestNewThrottledProgress_NilSink(t *testing.T) {
	p := NewThrottledProgress(nil, nil)
	// Must not panic.
	p(ProgressEvent{Stage: "index", Done: 1, Total: 2, Final: true})
}
