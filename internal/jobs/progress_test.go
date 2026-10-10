package jobs

import (
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
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
	p(ProgressEvent{Stage: ProgressStagePlan, Done: 0, Unit: "files"}) // first -> pass
	now = now.Add(500 * time.Millisecond)
	p(ProgressEvent{Stage: ProgressStagePlan, Done: 1, Unit: "files"}) // within 2s -> drop
	now = now.Add(500 * time.Millisecond)
	p(ProgressEvent{Stage: ProgressStagePlan, Done: 2, Unit: "files"}) // 1s -> drop
	now = now.Add(1100 * time.Millisecond)
	p(ProgressEvent{Stage: ProgressStagePlan, Done: 3, Unit: "files"}) // 2.1s -> pass
	now = now.Add(100 * time.Millisecond)
	p(ProgressEvent{Stage: ProgressStagePlan, Done: 4, Unit: "files", Final: true}) // final -> pass

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

	p(ProgressEvent{Stage: ProgressStagePlan, Done: 0, Total: 100, Unit: "files"}) // first -> pass (percent 0)
	now = now.Add(10 * time.Millisecond)
	p(ProgressEvent{Stage: ProgressStagePlan, Done: 1, Total: 100, Unit: "files"}) // percent 1 (+1) -> pass
	now = now.Add(10 * time.Millisecond)
	p(ProgressEvent{Stage: ProgressStagePlan, Done: 1, Total: 100, Unit: "files"}) // same percent, <2s -> drop

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
	p(ProgressEvent{Stage: ProgressStagePlan, Done: 1, Total: 2, Final: true})
}

func TestNewThrottledProgress_NonFinalNeverReportsOneHundredAndTotalGrows(t *testing.T) {
	var got []map[string]any
	p := NewThrottledProgress(func(m map[string]any) { got = append(got, m) }, func() time.Time { return time.Unix(0, 0) })
	p(ProgressEvent{Stage: ProgressStageEmbed, Done: 1, Total: 1, InFlight: 1, Unit: "files"})
	p(ProgressEvent{Stage: ProgressStageUpsert, Done: 2, Total: 2, Unit: "files", Flush: true})
	p(ProgressEvent{Stage: ProgressStagePrune, Done: 2, Total: 1, Unit: "files", Final: true})

	if len(got) != 3 {
		t.Fatalf("events=%d, want 3: %#v", len(got), got)
	}
	if got[0]["total"] != 2 || got[0]["percent"] != 50 {
		t.Fatalf("active event=%v, want evolved total=2 percent=50", got[0])
	}
	if got[1]["percent"].(int) >= 100 {
		t.Fatalf("non-final flush percent=%v, want <100", got[1]["percent"])
	}
	if got[2]["total"] != 2 || got[2]["percent"] != 100 {
		t.Fatalf("final event=%v, want exact total=2 percent=100", got[2])
	}
}

func TestSanitizeProgressCurrent(t *testing.T) {
	cases := []string{
		"", "a/b", `a\\b`, "\x1b[31msecret.md", "line\nbreak", "c1\u0085control", string([]byte{'x', 0xff, 'y'}),
		strings.Repeat("é", 65), "private-token-name.md",
	}
	for _, input := range cases {
		got := sanitizeProgressCurrent(input)
		if got == "" || !utf8.ValidString(got) || len(got) > maxProgressCurrentBytes {
			t.Errorf("sanitize(%q)=%q invalid or oversized", input, got)
		}
		for _, r := range got {
			if unicode.IsControl(r) || r == '/' || r == '\\' {
				t.Errorf("sanitize(%q)=%q retained forbidden rune %q", input, got, r)
			}
		}
	}
	if got := sanitizeProgressCurrent(""); got != "_" {
		t.Fatalf("sanitize(empty)=%q, want underscore placeholder", got)
	}
}

func TestSanitizeProgressCurrentMultibyteByteBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantLen int
	}{
		{name: "127 bytes", input: strings.Repeat("a", 125) + "é", wantLen: 127},
		{name: "128 bytes", input: strings.Repeat("a", 126) + "é", wantLen: 128},
		{name: "129 bytes drops whole final rune", input: strings.Repeat("a", 127) + "é", wantLen: 127},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeProgressCurrent(tt.input)
			if !utf8.ValidString(got) || len(got) != tt.wantLen {
				t.Fatalf("sanitize(%d-byte input)=%q (%d bytes), want valid UTF-8 and %d bytes", len(tt.input), got, len(got), tt.wantLen)
			}
			if strings.HasSuffix(got, string(utf8.RuneError)) {
				t.Fatalf("sanitize(%d-byte input) ended in replacement rune: %q", len(tt.input), got)
			}
		})
	}
}

func TestProgressStageVocabulary(t *testing.T) {
	got := []ProgressStage{ProgressStagePlan, ProgressStageExtract, ProgressStageEmbed, ProgressStageUpsert, ProgressStagePrune}
	want := []ProgressStage{"plan", "extract", "embed", "upsert", "prune"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stage[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}
