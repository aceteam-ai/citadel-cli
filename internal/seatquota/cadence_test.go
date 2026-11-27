package seatquota

import (
	"sync"
	"testing"
	"time"
)

func TestReadCadenceSerializesAndKeepsFailedAttemptFloor(t *testing.T) {
	c, err := NewReadCadence("seat-a", "codex")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	first, ok := c.Begin(at, false, 0)
	if !ok {
		t.Fatal("initial idle read not due")
	}
	if _, ok := c.Begin(at.Add(time.Hour), true, 0); ok {
		t.Fatal("overlapping read admitted")
	}
	// The supplied clock must not move backward even after a refused read.
	finished := at.Add(time.Hour)
	if err := c.Complete(first, time.Time{}, finished); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Begin(at.Add(time.Second), true, 0); ok {
		t.Fatal("clock rollback bypassed cadence")
	}
	// A failure did not fabricate an observation or erase the attempt time.
	s := c.Snapshot()
	if !s.LastAttempt.Equal(at) || !s.LastObservation.IsZero() || s.InFlight {
		t.Fatalf("unexpected failed-read state: %+v", s)
	}
	second, ok := c.Begin(finished, true, 0)
	if !ok {
		t.Fatal("retry after long failed read refused")
	}
	if err := c.Complete(second, time.Time{}, finished); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Begin(finished.Add(59*time.Second), true, 0); ok {
		t.Fatal("failure erased 60-second attempt floor")
	}
	if _, ok := c.Begin(finished.Add(time.Minute), true, 0); !ok {
		t.Fatal("attempt floor never releases")
	}
}

func TestReadCadenceCompletionAndPushOrdering(t *testing.T) {
	c, _ := NewReadCadence("seat-a", "codex")
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	read, _ := c.Begin(at, false, 0)
	if err := c.Observe(at.Add(20*time.Second), at.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	// A read started earlier must not replace a more recent push receipt.
	if err := c.Complete(read, at.Add(10*time.Second), at.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := c.Snapshot().LastObservation; !got.Equal(at.Add(20 * time.Second)) {
		t.Fatal("late read moved observation backward", got)
	}
	if _, ok := c.Begin(at.Add(140*time.Second), true, 0); ok {
		t.Fatal("fresh boundary triggered on-demand read")
	}
	next, ok := c.Begin(at.Add(141*time.Second), true, 0)
	if !ok {
		t.Fatal("aged observation did not trigger on-demand read")
	}
	if err := c.Complete(read, time.Time{}, at.Add(142*time.Second)); err != ErrInput {
		t.Fatal("stale completion accepted", err)
	}
	if !c.Snapshot().InFlight {
		t.Fatal("stale completion released another read")
	}
	if err := c.Complete(next, time.Time{}, at.Add(142*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestReadCadenceIsBoundToSeatAndProvider(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	a, _ := NewReadCadence("seat-a", "muse")
	b, _ := NewReadCadence("seat-b", "muse")
	read, ok := a.Begin(at, false, -time.Minute)
	if !ok {
		t.Fatal("idle-capable provider refused")
	}
	if err := b.Complete(read, at, at); err != ErrInput {
		t.Fatal("foreign attempt accepted", err)
	}
	if !a.Snapshot().InFlight || b.Snapshot().InFlight {
		t.Fatal("cross-seat state changed")
	}
	if err := a.Complete(read, time.Time{}, at); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Begin(at.Add(14*time.Minute-time.Nanosecond), false, -time.Minute); ok {
		t.Fatal("jittered idle floor ignored")
	}
	if _, ok := a.Begin(at.Add(14*time.Minute), false, -time.Minute); !ok {
		t.Fatal("jittered idle deadline missed")
	}
	for _, provider := range []string{"claude", "copilot"} {
		c, err := NewReadCadence("seat-a", provider)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := c.Begin(at, true, 0); ok {
			t.Fatal("turn-only provider admitted idle collection", provider)
		}
		if err := c.Observe(at, at); err != nil || !c.Snapshot().LastObservation.Equal(at) {
			t.Fatal("turn-only observation lost", provider, err)
		}
	}
}

func TestReadCadenceConcurrentBeginHasOneWinner(t *testing.T) {
	c, _ := NewReadCadence("seat-a", "codex")
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	winners := make(chan *ReadAttempt, 32)
	var wg sync.WaitGroup
	for i := 0; i < cap(winners); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if attempt, ok := c.Begin(at, true, 0); ok {
				winners <- attempt
			}
		}()
	}
	wg.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatal("concurrent reads admitted", len(winners))
	}
	if err := c.Complete(<-winners, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestReadCadenceInvalidInputDoesNotMutateState(t *testing.T) {
	for _, args := range [][2]string{{"", "codex"}, {"seat\nprivate", "codex"}, {"seat-a", "unsupported"}} {
		if _, err := NewReadCadence(args[0], args[1]); err != ErrInput {
			t.Fatal("invalid binding accepted", err)
		}
	}
	c, _ := NewReadCadence("seat-a", "codex")
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	var unbound ReadCadence
	if _, ok := unbound.Begin(at, true, 0); ok {
		t.Fatal("unbound zero value admitted a read")
	}
	if err := unbound.Observe(at, at); err != ErrInput {
		t.Fatal("unbound zero value recorded an observation", err)
	}
	if _, ok := c.Begin(at, false, time.Minute+1); ok {
		t.Fatal("unbounded jitter admitted")
	}
	if _, ok := c.Begin(time.Time{}, true, 0); ok {
		t.Fatal("missing clock admitted")
	}
	read, ok := c.Begin(at, false, 0)
	if !ok {
		t.Fatal("invalid request changed next admission")
	}
	for _, observed := range []time.Time{at.Add(-time.Second), at.Add(time.Hour)} {
		if err := c.Complete(read, observed, at); err != ErrInput || !c.Snapshot().InFlight {
			t.Fatal("invalid read receipt mutated state", err)
		}
	}
	if err := c.Complete(&ReadAttempt{}, at, at); err != ErrInput {
		t.Fatal("unissued attempt accepted", err)
	}
	if err := c.Complete(nil, at, at); err != ErrInput || !c.Snapshot().InFlight {
		t.Fatal("nil attempt mutated state", err)
	}
	if err := c.Observe(at.Add(time.Second), at); err != ErrInput {
		t.Fatal("future observation accepted", err)
	}
	if err := c.Complete(read, at, at); err != nil {
		t.Fatal(err)
	}
	if err := c.Observe(at.Add(-time.Second), at.Add(time.Second)); err != nil || !c.Snapshot().LastObservation.Equal(at) {
		t.Fatal("old observation changed newest receipt", err)
	}
}

func TestReadCadenceRejectsInvalidClocksAndCopiesSnapshots(t *testing.T) {
	c, _ := NewReadCadence("seat-a", "codex")
	at := time.Date(2030, 1, 1, 1, 0, 0, 0, time.FixedZone("synthetic", 3600))
	for _, invalid := range []time.Time{time.Time{}, time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)} {
		if _, ok := c.Begin(invalid, true, 0); ok {
			t.Fatal("invalid clock admitted a read")
		}
		if err := c.Observe(invalid, at); err != ErrInput {
			t.Fatal("invalid observation time accepted", err)
		}
	}
	read, ok := c.Begin(at, false, 0)
	if !ok {
		t.Fatal("valid attempt refused after invalid inputs")
	}
	if err := c.Complete(read, at, at); err != nil {
		t.Fatal(err)
	}
	view := c.Snapshot()
	if view.SeatRef != "seat-a" || view.Provider != "codex" || view.LastAttempt.Location() != time.UTC || view.LastObservation.Location() != time.UTC {
		t.Fatal("snapshot lost binding or UTC normalization", view)
	}
	view.LastAttempt = time.Time{}
	view.LastObservation = time.Time{}
	view.SeatRef = "seat-b"
	if actual := c.Snapshot(); actual.LastAttempt.IsZero() || actual.LastObservation.IsZero() || actual.SeatRef != "seat-a" {
		t.Fatal("snapshot mutation changed internal state", actual)
	}
	if err := c.Observe(at, at.Add(-time.Second)); err != ErrInput {
		t.Fatal("rollback receipt accepted", err)
	}
}
