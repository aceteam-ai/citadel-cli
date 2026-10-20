package ops

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLimitsAndSpansAreRefused(t *testing.T) {
	for _, limits := range []Limits{{MaxKeys: -1}, {MaxEventsPerKey: -1}} {
		if _, err := NewWindow(time.Second, limits); err == nil {
			t.Fatalf("accepted negative limits: %+v", limits)
		}
		if _, err := NewDedup(time.Second, limits); err == nil {
			t.Fatalf("accepted negative dedup limits: %+v", limits)
		}
		if _, err := NewRate(1, time.Second, limits); err == nil {
			t.Fatalf("accepted negative rate limits: %+v", limits)
		}
		if _, err := NewAbsence(time.Second, limits); err == nil {
			t.Fatalf("accepted negative absence limits: %+v", limits)
		}
		if _, err := NewSequence(2, time.Second, limits); err == nil {
			t.Fatalf("accepted negative sequence limits: %+v", limits)
		}
	}
	for _, span := range []time.Duration{0, -1} {
		if _, err := NewWindow(span, Limits{}); err == nil {
			t.Fatal("accepted invalid sliding span")
		}
		if _, err := NewTumblingWindow(span, Limits{}); err == nil {
			t.Fatal("accepted invalid tumbling span")
		}
		if _, err := NewDedup(span, Limits{}); err == nil {
			t.Fatal("accepted invalid dedup span")
		}
		if _, err := NewAbsence(span, Limits{}); err == nil {
			t.Fatal("accepted invalid absence span")
		}
		if _, err := NewSequence(2, span, Limits{}); err == nil {
			t.Fatal("accepted invalid sequence span")
		}
	}
	for _, steps := range []int{0, 1, 65} {
		if _, err := NewSequence(steps, time.Second, Limits{}); err == nil {
			t.Fatalf("accepted %d steps", steps)
		}
	}
	for _, tc := range []struct {
		capacity uint64
		interval time.Duration
	}{{0, 1}, {1, 0}, {1, -1}, {math.MaxUint64, 1}, {2, time.Duration(math.MaxInt64)}} {
		if _, err := NewRate(tc.capacity, tc.interval, Limits{}); err == nil {
			t.Fatalf("accepted invalid rate: %+v", tc)
		}
	}
	w, err := NewWindow(time.Second, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if w.state.limits.MaxKeys != DefaultMaxKeys || w.state.limits.MaxEventsPerKey != DefaultMaxEventsPerKey {
		t.Fatal("defaults not applied")
	}
}

func TestWindowBoundaryRingAndEvictionAccounting(t *testing.T) {
	w, err := NewWindow(10*time.Nanosecond, Limits{MaxKeys: 2, MaxEventsPerKey: 3})
	if err != nil {
		t.Fatal(err)
	}
	for at := int64(0); at < 4; at++ {
		w.Observe("a", string(rune('a'+at)), at)
	}
	got := w.Read("a", 3)
	if got.Count != 3 || got.Distinct != 3 || got.WindowStart != 1 {
		t.Fatalf("wrapped ring: %+v", got)
	}
	if s := w.Stats(); s.EventEvictions != 1 || s.StateEvicted != 1 {
		t.Fatalf("unreported event eviction: %+v", s)
	}
	got = w.Observe("a", "fifth", 11)
	if got.Count != 3 || got.WindowStart != 2 {
		t.Fatalf("expiry before append: %+v", got)
	}
	got = w.Read("a", 12)
	if got.Count != 2 || got.WindowStart != 3 {
		t.Fatalf("open left boundary: %+v", got)
	}
	w.Observe("b", "b", 12)
	w.Read("a", 12) // Inspection must not protect a key from LRU eviction.
	w.Observe("c", "c", 12)
	if got := w.Read("a", 12); got.Count != 0 {
		t.Fatalf("oldest key retained: %+v", got)
	}
	if s := w.Stats(); s.KeyEvictions != 1 || s.StateEvicted != 2 || s.ActiveKeys != 2 {
		t.Fatalf("unreported key eviction: %+v", s)
	}
	w.Advance(22)
	if s := w.Stats(); s.ActiveKeys != 0 || s.StateEvicted != 2 {
		t.Fatalf("natural expiry counted as loss: %+v", s)
	}
}

func TestWindowRejectsInvalidInputWithoutMutatingState(t *testing.T) {
	w, _ := NewWindow(time.Second, Limits{})
	w.Observe("a", "value", 20)
	for _, got := range []Snapshot{w.Observe("a", "bad", 19), w.Read("a", -1), w.Observe(strings.Repeat("k", MaxKeyBytes+1), "bad", 21), w.Observe("a", strings.Repeat("v", MaxValueBytes+1), 21)} {
		if !got.Rejected {
			t.Fatalf("invalid input accepted: %+v", got)
		}
	}
	w.Advance(19)
	if got := w.Read("a", 20); got.Count != 1 || got.Distinct != 1 {
		t.Fatalf("rejected input changed state: %+v", got)
	}
	if s := w.Stats(); s.InputRejected != 5 || s.ActiveKeys != 1 {
		t.Fatalf("missing rejection accounting: %+v", s)
	}
}

func TestTumblingAnchorsAndMaximumTimestamp(t *testing.T) {
	w, _ := NewTumblingWindow(10*time.Nanosecond, Limits{})
	if got := w.Observe("a", "x", 9); got.Count != 1 || got.WindowStart != 0 {
		t.Fatalf("first bucket: %+v", got)
	}
	if got := w.Observe("a", "x", 10); got.Count != 1 || got.WindowStart != 10 {
		t.Fatalf("new bucket: %+v", got)
	}
	if got := w.Read("absent", 19); got.Count != 0 || got.WindowStart != 10 {
		t.Fatalf("empty bucket: %+v", got)
	}
	w.Advance(20)
	if w.Stats().ActiveKeys != 0 || w.Stats().StateEvicted != 0 {
		t.Fatal("tumbling expiry was not natural")
	}
	if got := w.Observe("max", "x", math.MaxInt64); got.Rejected || got.WindowStart != (int64(math.MaxInt64)/10)*10 {
		t.Fatalf("maximum time overflowed: %+v", got)
	}
	sliding, _ := NewWindow(time.Duration(math.MaxInt64), Limits{})
	sliding.Observe("a", "zero", 0)
	if got := sliding.Read("a", math.MaxInt64); got.Count != 0 {
		t.Fatalf("exact maximum-span boundary: %+v", got)
	}
}

func TestDistinctSwitchIsDeterministicAndExpires(t *testing.T) {
	a, _ := NewWindow(10000*time.Nanosecond, Limits{})
	b, _ := NewWindow(10000*time.Nanosecond, Limits{})
	var first, second Snapshot
	for i := 0; i < 1500; i++ {
		value := strings.Repeat("x", i%8) + string(rune(i))
		first = a.Observe("a", value, int64(i))
		second = b.Observe("a", value, int64(i))
		if first != second {
			t.Fatalf("replay diverged at %d: %+v != %+v", i, first, second)
		}
		if i < exactDistinctLimit && (first.Approximate || first.Distinct != uint64(i+1)) {
			t.Fatalf("lost exactness early: %+v", first)
		}
		if i == exactDistinctLimit && !first.Approximate {
			t.Fatal("did not record sketch switch at 1,025 distinct values")
		}
	}
	if !first.Approximate || first.Distinct < 1200 || first.Distinct > 1500 {
		t.Fatalf("unexpected sketch estimate: %+v", first)
	}
	if a.Stats().ApproximateTransitions != 1 || a.Stats() != b.Stats() {
		t.Fatalf("switch accounting: %+v %+v", a.Stats(), b.Stats())
	}
	if got := a.Read("a", 10500); got.Approximate || got.Count != 999 || got.Distinct != 999 {
		t.Fatalf("expired values remained in sketch: %+v", got)
	}
	for i := int64(10501); i < 10530; i++ {
		a.Observe("a", string(rune(i)), i)
	}
	// Natural expiry and new values may keep this window below the switch.
	if a.Stats().StateEvicted != 0 {
		t.Fatal("sketch conversion counted as capacity loss")
	}
}

func TestRepeatedDistinctValueStaysExact(t *testing.T) {
	w, _ := NewWindow(time.Second, Limits{})
	for i := 0; i < 2000; i++ {
		w.Observe("a", "same", int64(i))
	}
	if got := w.Read("a", 2000); got.Count != 2000 || got.Distinct != 1 || got.Approximate {
		t.Fatalf("duplicates inflated distinct count: %+v", got)
	}
}

func TestDedupHasFixedAdmissionWindowAndLRUCap(t *testing.T) {
	d, _ := NewDedup(10*time.Nanosecond, Limits{MaxKeys: 2})
	if !d.Allow("a", 0) || d.Allow("a", 9) || !d.Allow("a", 10) {
		t.Fatal("dedup window extended or boundary was closed")
	}
	if !d.Allow("b", 10) || d.Allow("a", 10) || !d.Allow("c", 10) {
		t.Fatal("dedup admission failed")
	}
	if _, ok := d.state.get("b"); ok {
		t.Fatal("least recently seen key was not evicted")
	}
	if d.Allow("a", 9) || d.Allow(strings.Repeat("x", MaxKeyBytes+1), 11) {
		t.Fatal("invalid input allowed")
	}
	if s := d.Stats(); s.KeyEvictions != 1 || s.StateEvicted != 1 || s.InputRejected != 2 {
		t.Fatalf("dedup accounting: %+v", s)
	}
}

func TestRateIntegerRefillDoesNotBorrowFutureTokens(t *testing.T) {
	r, _ := NewRate(2, 10*time.Nanosecond, Limits{MaxKeys: 1})
	if !r.Allow("a", 0) || !r.Allow("a", 0) || r.Allow("a", 0) || r.Allow("a", 9) {
		t.Fatal("initial bucket capacity incorrect")
	}
	if !r.Allow("a", 10) || r.Allow("a", 19) || !r.Allow("a", 20) {
		t.Fatal("integer refill incorrect")
	}
	if !r.Allow("b", 20) || r.Stats().StateEvicted != 1 {
		t.Fatal("rate key cap did not report eviction")
	}
	if r.Allow("b", 19) {
		t.Fatal("regressing time allowed")
	}
	if !r.Allow("b", math.MaxInt64) {
		t.Fatal("maximum timestamp overflowed refill")
	}
}

func TestAbsenceEmitsOncePerEpisodeInStableOrder(t *testing.T) {
	a, _ := NewAbsence(10*time.Nanosecond, Limits{MaxKeys: 2})
	a.Observe("z", 0)
	a.Observe("a", 0)
	if got := a.Tick(9); len(got) != 0 {
		t.Fatalf("early absence: %+v", got)
	}
	want := []Missing{{Key: "a", LastSeen: 0, Deadline: 10}, {Key: "z", LastSeen: 0, Deadline: 10}}
	if got := a.Tick(10); !reflect.DeepEqual(got, want) {
		t.Fatalf("unstable absence: %+v", got)
	}
	if got := a.Tick(11); len(got) != 0 {
		t.Fatalf("repeated silence emitted twice: %+v", got)
	}
	a.Observe("a", 11)
	a.Observe("b", 11) // z is least recently seen.
	if s := a.Stats(); s.StateEvicted != 1 || s.KeyEvictions != 1 {
		t.Fatalf("absence cap: %+v", s)
	}
	if got := a.Tick(21); !reflect.DeepEqual(got, []Missing{{Key: "a", LastSeen: 11, Deadline: 21}, {Key: "b", LastSeen: 11, Deadline: 21}}) {
		t.Fatalf("new silence episode: %+v", got)
	}
	if a.Observe("a", 20) || len(a.Tick(20)) != 0 || a.Stats().InputRejected != 2 {
		t.Fatal("absence accepted regressing time")
	}
}

func TestSequencePreservesOverlappingStartsAndRepeatedIntermediates(t *testing.T) {
	s, _ := NewSequence(3, 10*time.Nanosecond, Limits{})
	for _, event := range []struct {
		step int
		at   int64
	}{{0, 1}, {1, 2}, {0, 3}, {1, 3}, {1, 4}} {
		if s.Observe("a", event.step, event.at).Matched {
			t.Fatal("sequence matched before final step")
		}
	}
	if got := s.Observe("a", 2, 5); !got.Matched || got.WindowStart != 3 {
		t.Fatalf("latest start did not dominate same stage: %+v", got)
	}
	if got := s.Observe("a", 2, 6); got.Matched {
		t.Fatalf("completed candidate reused: %+v", got)
	}
	other, _ := NewSequence(3, 10*time.Nanosecond, Limits{})
	other.Observe("a", 0, 1)
	other.Observe("a", 1, 2)
	other.Observe("a", 0, 3)
	if got := other.Observe("a", 2, 4); !got.Matched || got.WindowStart != 1 {
		t.Fatalf("A1 B2 A3 C4 lost older valid partial: %+v", got)
	}
	other.Observe("a", 1, 5)
	if got := other.Observe("a", 2, 6); !got.Matched || got.WindowStart != 3 {
		t.Fatalf("completion erased newer start: %+v", got)
	}
}

func TestSequenceTiesPreserveInputOrderAndInclusiveDeadline(t *testing.T) {
	s, _ := NewSequence(3, 10*time.Nanosecond, Limits{})
	s.Observe("a", 0, 1)
	s.Observe("a", 1, 1)
	s.Observe("a", 0, 1)
	if got := s.Observe("a", 2, 1); !got.Matched {
		t.Fatal("equal timestamps lost input ordering")
	}
	s.Observe("a", 1, 1)
	if got := s.Observe("a", 2, 11); !got.Matched {
		t.Fatal("inclusive deadline or newer tied partial lost")
	}
	s.Observe("a", 0, 12)
	s.Observe("a", 1, 13)
	if got := s.Observe("a", 2, 23); got.Matched {
		t.Fatal("expired sequence matched")
	}
	if !s.Observe("a", -1, 23).Rejected || !s.Observe("a", 3, 23).Rejected || !s.Observe("a", 0, 22).Rejected {
		t.Fatal("invalid sequence input accepted")
	}
}

func TestSequenceKeyCapAndNoCrossKeyCompletion(t *testing.T) {
	s, _ := NewSequence(2, time.Second, Limits{MaxKeys: 1})
	s.Observe("a", 0, 0)
	if got := s.Observe("b", 1, 1); got.Matched {
		t.Fatal("sequence crossed group keys")
	}
	s.Observe("b", 0, 1)
	if s.Stats().StateEvicted != 1 || s.Stats().ActiveKeys != 1 {
		t.Fatal("sequence cap not enforced")
	}
	if got := s.Observe("a", 1, 2); got.Matched {
		t.Fatal("evicted sequence matched")
	}
	if got := s.Observe("b", 1, 2); !got.Matched {
		t.Fatal("retained sequence did not match")
	}
	if s.Stats().ActiveKeys != 0 {
		t.Fatal("consumed sequence retained empty state")
	}
}

func BenchmarkWindowObserve(b *testing.B) {
	w, _ := NewWindow(time.Hour, Limits{MaxKeys: 100, MaxEventsPerKey: 4096})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.Observe("synthetic", "repeated-user", int64(i))
	}
}

func BenchmarkSequenceObserve(b *testing.B) {
	s, _ := NewSequence(3, time.Minute, Limits{MaxKeys: 100})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Observe("synthetic", i%3, int64(i))
	}
}
