package hostmetrics

import (
	"errors"
	"math"
	"strconv"
	"sync"
)

// JobTracker accepts only usage already attributed to the exact active job by
// trusted lifecycle code. It never divides host usage among jobs or guesses
// ownership of a shared GPU. There is no production observer wiring in S1.
type JobTracker struct {
	mu       sync.Mutex
	capacity int
	slots    []jobSlot
}
type jobSlot struct {
	id, kind, lane string
	generation     uint64
	cpu, rss       *float64
	lastCPU        float64
	cpuSeen        bool
}

// JobBinding is private local correlation data, deliberately separate from the
// Prometheus text. Slot+generation identifies one use of a reusable job slot.
// Job IDs/kinds/lanes must never become dynamic time-series labels.
type JobBinding struct {
	Slot              int
	Generation        uint64
	JobID, Kind, Lane string
}

func NewJobTracker(capacity int) (*JobTracker, error) {
	if capacity < 1 || capacity > 64 {
		return nil, errors.New("invalid job capacity")
	}
	return &JobTracker{capacity: capacity, slots: make([]jobSlot, capacity)}, nil
}
func (j *JobTracker) Begin(id, kind, lane string) (JobBinding, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(id) < 1 || len(id) > 128 {
		return JobBinding{}, errors.New("invalid job identity")
	}
	switch kind {
	case "inference", "meeting", "flow", "file", "container", "other":
	default:
		return JobBinding{}, errors.New("invalid job kind")
	}
	switch lane {
	case "general", "heavy", "inference", "realtime":
	default:
		return JobBinding{}, errors.New("invalid job lane")
	}
	for _, s := range j.slots {
		if s.id == id {
			return JobBinding{}, errors.New("job already tracked")
		}
	}
	for i, s := range j.slots {
		if s.id == "" && s.generation < (1<<53)-1 {
			j.slots[i] = jobSlot{id: id, kind: kind, lane: lane, generation: s.generation + 1}
			return JobBinding{i, s.generation + 1, id, kind, lane}, nil
		}
	}
	return JobBinding{}, errors.New("job capacity exhausted")
}
func (j *JobTracker) current(b JobBinding) bool {
	return b.Slot >= 0 && b.Slot < len(j.slots) && j.slots[b.Slot].id == b.JobID && b.JobID != "" && j.slots[b.Slot].generation == b.Generation
}

// Observe copies optional usage. Unknown CPU/RSS stays absent, not zero.
func (j *JobTracker) Observe(b JobBinding, cpuSeconds, residentBytes *float64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.current(b) {
		return errors.New("stale job binding")
	}
	for _, n := range []*float64{cpuSeconds, residentBytes} {
		if n != nil && (math.IsNaN(*n) || math.IsInf(*n, 0) || *n < 0) {
			return errors.New("invalid job usage")
		}
	}
	s := &j.slots[b.Slot]
	if s.cpuSeen && cpuSeconds != nil && *cpuSeconds < s.lastCPU {
		return errors.New("job CPU counter moved backwards")
	}
	clone := func(n *float64) *float64 {
		if n == nil {
			return nil
		}
		v := *n
		return &v
	}
	if cpuSeconds != nil {
		// An unknown export must not erase validation history for this generation.
		s.lastCPU, s.cpuSeen = *cpuSeconds, true
	}
	s.cpu, s.rss = clone(cpuSeconds), clone(residentBytes)
	return nil
}
func (j *JobTracker) End(b JobBinding) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.current(b) {
		return errors.New("stale job binding")
	}
	j.slots[b.Slot] = jobSlot{generation: b.Generation}
	return nil
}
func (j *JobTracker) Snapshot() (Group, []JobBinding) {
	j.mu.Lock()
	defer j.mu.Unlock()
	group := Group{Known: true}
	var bindings []JobBinding
	for i, s := range j.slots {
		key := strconv.Itoa(i)
		active := 0.0
		if s.id != "" {
			active = 1
		}
		group.Readings = append(group.Readings, Reading{JobActive, key, active}, Reading{JobGeneration, key, float64(s.generation)})
		if s.id == "" {
			continue
		}
		bindings = append(bindings, JobBinding{i, s.generation, s.id, s.kind, s.lane})
		if s.cpu != nil {
			group.Readings = append(group.Readings, Reading{JobCPUSeconds, key, *s.cpu})
		}
		if s.rss != nil {
			group.Readings = append(group.Readings, Reading{JobResidentBytes, key, *s.rss})
		}
	}
	return group, bindings
}
