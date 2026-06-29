package worker

import "testing"

// TestFetchErrLogLevel verifies the coalescing policy that keeps self-healing
// consume failures from flooding the activity log: quiet first blip, silence
// through brief streaks, one warning when sustained, then sparse re-warns.
func TestFetchErrLogLevel(t *testing.T) {
	const threshold, repeat = 5, 10

	cases := []struct {
		consecutive int
		wantLevel   string
		wantLog     bool
	}{
		{1, "info", true},     // first blip: quiet info
		{2, "", false},        // brief streak: silent
		{3, "", false},        // silent
		{4, "", false},        // silent
		{5, "warning", true},  // sustained: escalate once
		{6, "", false},        // silent again
		{14, "", false},       // silent
		{15, "warning", true}, // threshold(5) + repeat(10): re-warn
		{16, "", false},       // silent
		{25, "warning", true}, // next re-warn
	}

	for _, c := range cases {
		level, ok := fetchErrLogLevel(c.consecutive, threshold, repeat)
		if ok != c.wantLog || level != c.wantLevel {
			t.Errorf("fetchErrLogLevel(%d) = (%q,%v), want (%q,%v)",
				c.consecutive, level, ok, c.wantLevel, c.wantLog)
		}
	}
}

// TestFetchErrLogLevelZeroRepeat ensures a zero repeat never divides by zero
// and simply stops re-warning after the initial escalation.
func TestFetchErrLogLevelZeroRepeat(t *testing.T) {
	if _, ok := fetchErrLogLevel(100, 5, 0); ok {
		t.Error("expected no log when repeat is 0 and past threshold")
	}
	if level, ok := fetchErrLogLevel(5, 5, 0); !ok || level != "warning" {
		t.Errorf("threshold escalation should still fire with repeat=0; got (%q,%v)", level, ok)
	}
}
