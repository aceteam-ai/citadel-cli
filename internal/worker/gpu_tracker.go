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

// gpuBoundJobTypes are the job types that actually dispatch to a node-local
// GPU inference engine (vLLM/llama.cpp/Ollama/bonsai/...) and so meaningfully
// contend for a GPU slot. Everything else -- SERVICE_START, shell, file,
// config, etc. -- runs through processJob without ever touching engine VRAM,
// so it must never wait on (or be Nacked by) the GPU-slot semaphore.
//
// citadel-cli#825: MaxConcurrency defaults to the GPU count but is an
// operator-settable flag (cmd/work.go). Set above the GPU count, more jobs can
// be in flight than GPU slots, so a non-GPU job racing concurrent inference
// jobs could reach the "no GPU slots available" Nack branch in processJob and
// Nack with ZERO published terminal events -- the backend's stream:v1:{jobId}
// waiter then times out and degrades to polling, reproducing the #559 symptom
// via GPU contention instead of a publish failure. Gating the acquire on this
// predicate removes non-GPU jobs from that path entirely rather than trying to
// give the Nack a distinguishable terminal event (a real inference Nack must
// stay a transparent, non-terminal retry -- see the #559 note on processJob).
//
// Deliberately excluded, even though their handlers also make an outbound HTTP
// call to a local engine/sidecar: JobTypeEmbedding (TEI), JobTypeExtraction
// (GLiNER2), JobTypeTranscribeAudio (faster-whisper), JobTypeSynthesizeSpeech
// (kokoro). None of them share the vLLM/llama.cpp/Ollama/bonsai chat-completion
// GPU-serving path this tracker models, and the issue's request was scoped to
// "inference/llm_inference and similar". If one of these sidecars turns out to
// genuinely contend for the same GPU in practice, add it here explicitly rather
// than broadening the predicate implicitly.
var gpuBoundJobTypes = map[string]struct{}{
	JobTypeLlamaCppInference: {},
	JobTypeVLLMInference:     {},
	JobTypeOllamaInference:   {},
	JobTypeLLMInference:      {}, // Redis worker format ("llm_inference"), all fabric inference (issue #590)
}

// needsGPUSlot reports whether jobType should acquire a GPU slot from the
// tracker before running. See gpuBoundJobTypes.
func needsGPUSlot(jobType string) bool {
	_, ok := gpuBoundJobTypes[jobType]
	return ok
}
