package worker

import "sync"

// GPUTracker manages GPU slot allocation for concurrent job execution.
// Thread-safe via mutex.
type GPUTracker struct {
	mu    sync.Mutex
	slots []bool // true = in use
}

// NewGPUTracker creates a tracker for the given number of GPUs.
func NewGPUTracker(gpuCount int) *GPUTracker {
	return &GPUTracker{slots: make([]bool, gpuCount)}
}

// Acquire returns the index of the first available GPU slot, or -1 if all are busy.
func (t *GPUTracker) Acquire() (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, inUse := range t.slots {
		if !inUse {
			t.slots[i] = true
			return i, true
		}
	}
	return -1, false
}

// AcquireSpecific attempts to acquire a specific GPU index.
// Returns false if the index is invalid or already in use.
func (t *GPUTracker) AcquireSpecific(index int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if index < 0 || index >= len(t.slots) || t.slots[index] {
		return false
	}
	t.slots[index] = true
	return true
}

// Release marks a GPU slot as available.
func (t *GPUTracker) Release(index int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if index >= 0 && index < len(t.slots) {
		t.slots[index] = false
	}
}

// AvailableCount returns the number of free GPU slots.
func (t *GPUTracker) AvailableCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := 0
	for _, inUse := range t.slots {
		if !inUse {
			count++
		}
	}
	return count
}

// Total returns the total number of GPU slots.
func (t *GPUTracker) Total() int {
	return len(t.slots)
}
