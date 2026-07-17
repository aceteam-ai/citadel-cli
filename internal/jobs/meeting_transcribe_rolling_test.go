package jobs

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// seg is a terse constructor for test segments.
func seg(start, end float64, text string) TranscriptSegment {
	return TranscriptSegment{Start: start, End: end, Text: text}
}

func startsOf(segs []TranscriptSegment) []float64 {
	out := make([]float64, len(segs))
	for i, s := range segs {
		out[i] = s.Start
	}
	return out
}

func TestStabilizedSegments_WindowHoldsBackTail(t *testing.T) {
	// Latest end is 30s; with a 10s window, everything ending after 20s is the
	// churning tail and must be withheld.
	segs := []TranscriptSegment{
		seg(0, 5, "a"),
		seg(5, 12, "b"),
		seg(12, 22, "c"), // ends at 22 > cutoff 20 -> withheld
		seg(22, 30, "d"), // tail -> withheld
	}
	emitted := map[int64]struct{}{}
	got := stabilizedSegments(segs, 10*time.Second, emitted)
	if want := []float64{0, 5}; !reflect.DeepEqual(startsOf(got), want) {
		t.Errorf("stabilized starts = %v, want %v", startsOf(got), want)
	}
}

func TestStabilizedSegments_ZeroWindowEmitsAll(t *testing.T) {
	segs := []TranscriptSegment{seg(0, 5, "a"), seg(5, 10, "b")}
	emitted := map[int64]struct{}{}
	got := stabilizedSegments(segs, 0, emitted)
	if len(got) != 2 {
		t.Errorf("zero window should emit all; got %d", len(got))
	}
}

func TestStabilizedSegments_Empty(t *testing.T) {
	if got := stabilizedSegments(nil, 0, map[int64]struct{}{}); got != nil {
		t.Errorf("empty input should yield nil, got %v", got)
	}
}

func TestStabilizedSegments_MonotonicGrowthNoDoubleEmit(t *testing.T) {
	emitted := map[int64]struct{}{}
	window := 10 * time.Second

	// Pass 1: recording up to 20s. Cutoff = 10s. Emit segments ending <= 10.
	p1 := []TranscriptSegment{seg(0, 5, "a"), seg(5, 10, "b"), seg(10, 20, "c")}
	got1 := stabilizedSegments(p1, window, emitted)
	if want := []float64{0, 5}; !reflect.DeepEqual(startsOf(got1), want) {
		t.Fatalf("pass1 starts = %v, want %v", startsOf(got1), want)
	}

	// Pass 2: recording grew to 40s; earlier segments unchanged, "c" now stable.
	p2 := []TranscriptSegment{seg(0, 5, "a"), seg(5, 10, "b"), seg(10, 20, "c"), seg(20, 32, "d"), seg(32, 40, "e")}
	got2 := stabilizedSegments(p2, window, emitted)
	// Cutoff = 30s: a,b already emitted; c (end 20) and d (end 32? >30 withheld).
	// So only "c" (start 10) is newly stable.
	if want := []float64{10}; !reflect.DeepEqual(startsOf(got2), want) {
		t.Fatalf("pass2 starts = %v, want %v (no re-emit of a/b, c newly stable)", startsOf(got2), want)
	}
}

func TestStabilizedSegments_RevisedTailEmittedOnceAfterStabilizing(t *testing.T) {
	emitted := map[int64]struct{}{}
	window := 5 * time.Second

	// Pass 1: tail segment at start 10 is churning (text "helo"), withheld.
	p1 := []TranscriptSegment{seg(0, 8, "hello there"), seg(10, 14, "helo")}
	got1 := stabilizedSegments(p1, window, emitted)
	if want := []float64{0}; !reflect.DeepEqual(startsOf(got1), want) {
		t.Fatalf("pass1 starts = %v, want %v", startsOf(got1), want)
	}

	// Pass 2: the tail was REVISED (start drifted 10.0 -> 10.3, text fixed) and
	// now more audio follows so it is stable. It must emit exactly once, keyed on
	// its coarse start bucket despite the drift.
	p2 := []TranscriptSegment{seg(0, 8, "hello there"), seg(10.3, 14.2, "hello"), seg(14.2, 25, "and more")}
	got2 := stabilizedSegments(p2, window, emitted)
	if want := []float64{10.3}; !reflect.DeepEqual(startsOf(got2), want) {
		t.Fatalf("pass2 starts = %v, want %v (revised tail emitted once)", startsOf(got2), want)
	}

	// Pass 3: same content re-transcribed; nothing new should emit.
	got3 := stabilizedSegments(p2, window, emitted)
	if len(got3) != 0 {
		t.Fatalf("pass3 should emit nothing; got %v", startsOf(got3))
	}
}

func TestRollingTranscriber_Run_EmitsAcrossPassesAndFinalizes(t *testing.T) {
	var mu sync.Mutex
	var emitted []TranscriptSegment

	// Fake transcriber: returns a growing segment list on each call.
	var passes int
	transcribe := func() ([]TranscriptSegment, error) {
		mu.Lock()
		defer mu.Unlock()
		passes++
		switch {
		case passes == 1:
			return []TranscriptSegment{seg(0, 5, "a"), seg(5, 20, "b")}, nil // b is tail
		case passes == 2:
			// Recording grew; b now stable, c is the new tail.
			return []TranscriptSegment{seg(0, 5, "a"), seg(5, 12, "b"), seg(12, 30, "c")}, nil
		default:
			// Grew again; c is now stable (a later segment d is the tail).
			return []TranscriptSegment{seg(0, 5, "a"), seg(5, 12, "b"), seg(12, 30, "c"), seg(30, 50, "d")}, nil
		}
	}

	rt := &RollingTranscriber{
		Interval:   5 * time.Millisecond,
		Window:     10 * time.Second,
		Transcribe: transcribe,
		Emit: func(s TranscriptSegment) {
			mu.Lock()
			emitted = append(emitted, s)
			mu.Unlock()
		},
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		rt.Run(stop)
		close(done)
	}()

	// Let several passes run, then stop.
	time.Sleep(60 * time.Millisecond)
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after stop")
	}

	mu.Lock()
	defer mu.Unlock()
	// Across all passes we expect a (start 0), b (start 5), c (start 12) each
	// exactly once, in order.
	if want := []float64{0, 5, 12}; !reflect.DeepEqual(startsOf(emitted), want) {
		t.Fatalf("emitted starts = %v, want %v", startsOf(emitted), want)
	}
}

// TestRollingTranscriber_Run_NoPeriodicPassAfterStop is the Bug B lifecycle
// guard (live-prod node 1084, 2026-07-16: rolling passes still firing 68s after
// "leaving"). Go's select picks a uniformly random ready case, so once stop is
// closed a still-firing ticker could keep winning and run more periodic passes.
// The priority re-check of stop must ensure that after stop closes, at most the
// in-flight pass completes plus exactly ONE final pass — never another periodic
// pass. We pin exactly one pass in flight when stop closes, then assert the total
// is 2 (in-flight + final), which fails on the old random-select behavior.
func TestRollingTranscriber_Run_NoPeriodicPassAfterStop(t *testing.T) {
	started := make(chan struct{}) // closed when the first pass begins
	release := make(chan struct{}) // gates the in-flight pass until we say go
	var passes int32
	var once sync.Once

	transcribe := func() ([]TranscriptSegment, error) {
		n := atomic.AddInt32(&passes, 1)
		if n == 1 {
			once.Do(func() { close(started) })
			<-release // hold the first pass in flight across close(stop)
		}
		return nil, nil
	}

	rt := &RollingTranscriber{
		Interval:   time.Millisecond, // ticker fires rapidly while pass 1 is held
		Window:     0,
		Transcribe: transcribe,
		Emit:       func(TranscriptSegment) {},
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { rt.Run(stop); close(done) }()

	<-started    // pass 1 is in flight (blocked in runPass)
	close(stop)  // stop while a pass is in flight and ticks have queued
	// Give a buggy random-select a chance to fire extra passes. It can't, because
	// Run is still blocked inside pass 1 — but this also lets the ticker buffer a
	// pending tick so the post-release select genuinely races stop vs ticker.
	time.Sleep(20 * time.Millisecond)
	close(release) // let pass 1 finish; Run must now do exactly one final pass

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after stop")
	}
	if got := atomic.LoadInt32(&passes); got != 2 {
		t.Errorf("passes = %d, want 2 (one in-flight + one final; no periodic pass after stop)", got)
	}
}

func TestRollingTranscriber_Run_PassErrorIsNonFatal(t *testing.T) {
	var mu sync.Mutex
	var emitted []TranscriptSegment
	var calls int
	transcribe := func() ([]TranscriptSegment, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("whisper hiccup")
		}
		return []TranscriptSegment{seg(0, 5, "ok")}, nil
	}
	rt := &RollingTranscriber{
		Interval:   5 * time.Millisecond,
		Window:     0,
		Transcribe: transcribe,
		Emit: func(s TranscriptSegment) {
			mu.Lock()
			emitted = append(emitted, s)
			mu.Unlock()
		},
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { rt.Run(stop); close(done) }()
	time.Sleep(40 * time.Millisecond)
	close(stop)
	<-done

	mu.Lock()
	defer mu.Unlock()
	// The first pass errored (non-fatal); a later pass succeeded and emitted.
	if len(emitted) == 0 {
		t.Fatal("expected a segment emitted after the transient error")
	}
}

func TestRollingTranscriber_Run_GuardsMisconfiguration(t *testing.T) {
	// Non-positive interval or missing callbacks must return immediately, not
	// spin or panic.
	for name, rt := range map[string]*RollingTranscriber{
		"zero interval":  {Interval: 0, Transcribe: func() ([]TranscriptSegment, error) { return nil, nil }, Emit: func(TranscriptSegment) {}},
		"nil transcribe": {Interval: time.Millisecond, Emit: func(TranscriptSegment) {}},
		"nil emit":       {Interval: time.Millisecond, Transcribe: func() ([]TranscriptSegment, error) { return nil, nil }},
	} {
		t.Run(name, func(t *testing.T) {
			done := make(chan struct{})
			go func() { rt.Run(make(chan struct{})); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("Run did not return immediately on misconfiguration")
			}
		})
	}
}
