package worker

import (
	"sync"
	"testing"
)

func TestGPUTracker_Acquire(t *testing.T) {
	tracker := NewGPUTracker(2)

	idx, ok := tracker.Acquire()
	if !ok || idx != 0 {
		t.Fatalf("expected GPU 0, got %d (ok=%v)", idx, ok)
	}

	idx, ok = tracker.Acquire()
	if !ok || idx != 1 {
		t.Fatalf("expected GPU 1, got %d (ok=%v)", idx, ok)
	}

	// All slots taken
	_, ok = tracker.Acquire()
	if ok {
		t.Fatal("expected Acquire to fail when all slots taken")
	}
}

func TestGPUTracker_Release(t *testing.T) {
	tracker := NewGPUTracker(1)

	idx, _ := tracker.Acquire()
	tracker.Release(idx)

	idx2, ok := tracker.Acquire()
	if !ok || idx2 != 0 {
		t.Fatalf("expected GPU 0 after release, got %d (ok=%v)", idx2, ok)
	}
}

func TestGPUTracker_AcquireSpecific(t *testing.T) {
	tracker := NewGPUTracker(3)

	if !tracker.AcquireSpecific(1) {
		t.Fatal("expected AcquireSpecific(1) to succeed")
	}
	if tracker.AcquireSpecific(1) {
		t.Fatal("expected AcquireSpecific(1) to fail (already acquired)")
	}
	if tracker.AcquireSpecific(5) {
		t.Fatal("expected AcquireSpecific(5) to fail (out of range)")
	}
}

func TestGPUTracker_AvailableCount(t *testing.T) {
	tracker := NewGPUTracker(4)
	if tracker.AvailableCount() != 4 {
		t.Fatalf("expected 4 available, got %d", tracker.AvailableCount())
	}

	tracker.Acquire()
	tracker.Acquire()
	if tracker.AvailableCount() != 2 {
		t.Fatalf("expected 2 available, got %d", tracker.AvailableCount())
	}
}

func TestGPUTracker_Total(t *testing.T) {
	tracker := NewGPUTracker(6)
	if tracker.Total() != 6 {
		t.Fatalf("expected total 6, got %d", tracker.Total())
	}
}

func TestGPUTracker_Concurrent(t *testing.T) {
	tracker := NewGPUTracker(4)
	var wg sync.WaitGroup
	acquired := make(chan int, 100)

	// 10 goroutines competing for 4 slots
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			idx, ok := tracker.Acquire()
			if ok {
				acquired <- idx
			}
		}()
	}
	wg.Wait()
	close(acquired)

	count := 0
	seen := make(map[int]bool)
	for idx := range acquired {
		count++
		if seen[idx] {
			t.Fatalf("GPU %d acquired twice", idx)
		}
		seen[idx] = true
	}
	if count != 4 {
		t.Fatalf("expected exactly 4 acquisitions, got %d", count)
	}
}

func TestGPUTracker_ReleaseInvalidIndex(t *testing.T) {
	tracker := NewGPUTracker(2)

	// Should not panic
	tracker.Release(-1)
	tracker.Release(5)
	tracker.Release(2)

	// All slots should still be available
	if tracker.AvailableCount() != 2 {
		t.Fatalf("expected 2 available after invalid releases, got %d", tracker.AvailableCount())
	}
}
