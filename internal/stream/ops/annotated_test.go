package ops

import (
	"testing"
	"time"
)

func TestAnnotatedWindowRetainsPriorUncertaintyUntilItsObservationExpires(t *testing.T) {
	w, err := NewWindow(10*time.Nanosecond, Limits{})
	if err != nil { t.Fatal(err) }
	first := w.ObserveAnnotated("key", "first", 0, true)
	if first.Rejected || first.Count != 1 || first.Distinct != 1 || !first.Approximate {
		t.Fatalf("upstream annotation was lost on first observation: %+v", first)
	}
	second := w.Observe("key", "second", 5)
	if second.Count != 2 || !second.Approximate { t.Fatalf("exact later observation erased prior uncertainty: %+v", second) }
	retained := w.Read("key", 9)
	if retained.Count != 2 || !retained.Approximate { t.Fatalf("uncertainty disappeared before expiry: %+v", retained) }
	expired := w.Read("key", 10)
	if expired.Count != 1 || expired.Distinct != 1 || expired.Approximate {
		t.Fatalf("expired upstream uncertainty contaminated the exact retained observation: %+v", expired)
	}
	if w.Stats().ApproximateTransitions != 0 || w.Stats().StateEvicted != 0 {
		t.Fatal("upstream annotation or natural expiry was counted as local sketch conversion/capacity loss")
	}
	w.Advance(15)
	if w.Stats().ActiveKeys != 0 { t.Fatal("watermark failed to expire final exact observation") }
	if fresh := w.Observe("key", "fresh", 16); fresh.Count != 1 || fresh.Approximate { t.Fatalf("new episode inherited stale uncertainty: %+v", fresh) }
}

func TestAnnotatedWindowCapacityEvictionAndTumblingResetClearOnlyLostAnnotations(t *testing.T) {
	w, err := NewWindow(time.Second, Limits{MaxEventsPerKey: 2})
	if err != nil { t.Fatal(err) }
	w.ObserveAnnotated("key", "approximate", 0, true)
	w.Observe("key", "exact-one", 1)
	if got := w.Observe("key", "exact-two", 2); got.Count != 2 || got.Approximate {
		t.Fatalf("ring eviction retained uncertainty from an overwritten event: %+v", got)
	}
	if w.Stats().EventEvictions != 1 || w.Stats().StateEvicted != 1 { t.Fatal("annotation ring eviction lost its capacity accounting") }
	tumbling, err := NewTumblingWindow(10*time.Nanosecond, Limits{})
	if err != nil { t.Fatal(err) }
	if first := tumbling.ObserveAnnotated("key", "old", 9, true); !first.Approximate { t.Fatal("missing first bucket annotation") }
	if next := tumbling.Observe("key", "new", 10); next.Count != 1 || next.WindowStart != 10 || next.Approximate {
		t.Fatalf("new tumbling bucket inherited old uncertainty: %+v", next)
	}
}

func TestAnnotatedSequenceCarriesUncertaintyFromAnyPriorStage(t *testing.T) {
	for uncertainStep := 0; uncertainStep < 3; uncertainStep++ {
		s, err := NewSequence(3, 10*time.Nanosecond, Limits{})
		if err != nil { t.Fatal(err) }
		for step := 0; step < 3; step++ {
			got := s.ObserveAnnotated("key", step, int64(step), step == uncertainStep)
			if got.Rejected || got.Matched != (step == 2) { t.Fatalf("bad annotated sequence progression: %+v", got) }
			if step == 2 && !got.Approximate { t.Fatalf("stage %d uncertainty disappeared at exact completion: %+v", uncertainStep, got) }
		}
		for step := 0; step < 3; step++ {
			got := s.Observe("key", step, int64(step+3))
			if step == 2 && (!got.Matched || got.Approximate) { t.Fatalf("next exact sequence inherited a consumed annotation: %+v", got) }
		}
	}
}

func TestAnnotatedSequenceOverlappingNewerStartDoesNotInheritOlderUncertainty(t *testing.T) {
	s, err := NewSequence(3, 10*time.Nanosecond, Limits{})
	if err != nil { t.Fatal(err) }
	s.ObserveAnnotated("key", 0, 0, true)
	s.Observe("key", 1, 1)
	s.Observe("key", 0, 2)
	if old := s.Observe("key", 2, 3); !old.Matched || !old.Approximate || old.WindowStart != 0 {
		t.Fatalf("older valid partial lost its uncertainty: %+v", old)
	}
	s.Observe("key", 1, 4)
	if newer := s.Observe("key", 2, 5); !newer.Matched || newer.Approximate || newer.WindowStart != 2 {
		t.Fatalf("newer exact start inherited a completed older annotation: %+v", newer)
	}
}

func TestAnnotatedSequenceExpiryDiscardsUncertaintyAndPartialCandidates(t *testing.T) {
	s, err := NewSequence(3, 10*time.Nanosecond, Limits{})
	if err != nil { t.Fatal(err) }
	s.ObserveAnnotated("key", 0, 0, true)
	s.Observe("key", 1, 1)
	s.Advance(11)
	if s.Stats().ActiveKeys != 0 || s.Stats().StateEvicted != 0 { t.Fatal("expired partial remained active or natural expiry counted as loss") }
	if stale := s.Observe("key", 2, 11); stale.Matched { t.Fatal("expired partial completed") }
	s.Observe("key", 0, 12)
	s.Observe("key", 1, 13)
	if fresh := s.Observe("key", 2, 14); !fresh.Matched || fresh.Approximate {
		t.Fatalf("fresh exact sequence inherited expired uncertainty: %+v", fresh)
	}
}
