package ops

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"math/bits"
	"time"
)

const exactDistinctLimit = 1024
const sketchRegisters = 1024

type observation struct {
	at int64
	hash [sha256.Size]byte
	approximate bool
}

type windowState struct {
	events []observation
	head int
	approximate bool
	bucket int64
}

// Snapshot summarizes only retained observations. Rejected marks invalid keys
// or regressing time; such input never mutates keyed state. WindowStart is the
// oldest retained observation (or at when empty) for sliding windows, and the
// epoch-zero bucket anchor for tumbling windows.
type Snapshot struct {
	Count uint64
	Distinct uint64
	Approximate bool
	Rejected bool
	WindowStart int64
}

// Window is a sliding window over (at-span, at]. It retains a bounded ring per
// key and hashes values before retaining them. Beyond the exact-distinct limit
// it uses deterministic HyperLogLog with 1,024 registers (about 3.25% relative
// standard error). The sketch is rebuilt from the active ring so expired values
// never remain in a non-expiring HLL. A key can return to exact mode as it ages.
type Window struct {
	state *cache[windowState]
	span int64
	tumbling bool
}

func NewWindow(span time.Duration, limits Limits) (*Window, error) {
	if err := validSpan(span); err != nil { return nil, err }
	s, err := newCache[windowState](limits)
	if err != nil { return nil, err }
	return &Window{state: s, span: int64(span)}, nil
}

// NewTumblingWindow uses epoch-zero buckets [k*span, (k+1)*span). All
// observations in one bucket report that bucket's start, including empty reads.
func NewTumblingWindow(span time.Duration, limits Limits) (*Window, error) {
	w, err := NewWindow(span, limits)
	if err != nil { return nil, err }
	w.tumbling = true
	return w, nil
}

func (w *Window) Observe(key, value string, at int64) Snapshot {
	return w.ObserveAnnotated(key,value,at,false)
}

// ObserveAnnotated retains uncertainty inherited from an upstream correlation.
// It is independent of whether this window's own distinct set uses a sketch.
func (w *Window) ObserveAnnotated(key,value string,at int64,approximate bool)Snapshot{
	if len(value) > MaxValueBytes {
		w.state.stats.InputRejected++
		return Snapshot{Rejected: true}
	}
	if !w.state.accept(key, at) { return Snapshot{Rejected: true} }
	w.expire(at)
	v := w.state.touch(key, at, windowState{})
	w.prune(&v.value, at)
	h := sha256.Sum256([]byte(value))
	event := observation{at: at, hash: h,approximate:approximate}
	if len(v.value.events) < w.state.limits.MaxEventsPerKey {
		v.value.events = append(v.value.events, event)
	} else {
		v.value.events[v.value.head] = event
		v.value.head = (v.value.head+1)%len(v.value.events)
		w.state.stats.EventEvictions++
		w.state.stats.StateEvicted++
	}
	return w.summarize(&v.value, at)
}

func (w *Window) Read(key string, at int64) Snapshot {
	if !w.state.accept(key, at) { return Snapshot{Rejected: true} }
	w.expire(at)
	v, ok := w.state.get(key)
	if !ok { return Snapshot{WindowStart: w.emptyStart(at)} }
	w.prune(&v.value, at)
	return w.summarize(&v.value, at)
}

// Advance expires keys without introducing observations or touching LRU order.
func (w *Window) Advance(at int64) {
	if w.state.advance(at) { w.expire(at) }
}

func (w *Window) Stats() Stats { return w.state.snapshot() }

func (w *Window) emptyStart(at int64) int64 {
	if w.tumbling { return (at/w.span)*w.span }
	return at
}

func (w *Window) expire(at int64) {
	if !w.tumbling { w.state.expire(at, w.span); return }
	start := w.emptyStart(at)
	if start > 0 { w.state.expire(start-1, 0) }
}

func (w *Window) prune(s *windowState, at int64) {
	if w.tumbling {
		start := w.emptyStart(at)
		if s.bucket != start {
			s.events = s.events[:0]
			s.head = 0
			s.approximate = false
			s.bucket = start
		}
		return
	}
	// Ring entries are chronological beginning at head. Compact after expiry;
	// the allocated storage remains capped, including when the ring has wrapped.
	if len(s.events) == 0 { return }
	first := 0
	for first < len(s.events) && s.events[(s.head+first)%len(s.events)].at <= at-w.span { first++ }
	if first == 0 { return }
	if first == len(s.events) {
		s.events = s.events[:0]
		s.head = 0
		return
	}
	retained := make([]observation, len(s.events)-first)
	for i := range retained { retained[i] = s.events[(s.head+first+i)%len(s.events)] }
	s.events = retained
	s.head = 0
}

func (w *Window) summarize(s *windowState, at int64) Snapshot {
	out := Snapshot{Count: uint64(len(s.events)), WindowStart: w.emptyStart(at)}
	if len(s.events) == 0 { s.approximate = false; return out }
	if !w.tumbling { out.WindowStart = s.events[s.head].at }
	exact := make(map[[sha256.Size]byte]struct{}, exactDistinctLimit+1)
	var registers [sketchRegisters]uint8
	for _, event := range s.events {
		out.Approximate=out.Approximate||event.approximate
		if len(exact) <= exactDistinctLimit { exact[event.hash] = struct{}{} }
		hash := binary.BigEndian.Uint64(event.hash[:8])
		index := hash & (sketchRegisters-1)
		rank := uint8(bits.LeadingZeros64(hash>>10)-10+1)
		if rank > registers[index] { registers[index] = rank }
	}
	if len(exact) <= exactDistinctLimit {
		out.Distinct = uint64(len(exact))
		s.approximate = false
		return out
	}
	out.Approximate = true
	if !s.approximate { w.state.stats.ApproximateTransitions++ }
	s.approximate = true
	var reciprocal float64
	zeros := 0
	for _, rank := range registers {
		reciprocal += math.Ldexp(1, -int(rank))
		if rank == 0 { zeros++ }
	}
	estimate := (0.7213/(1+1.079/sketchRegisters))*sketchRegisters*sketchRegisters/reciprocal
	if estimate <= 2.5*sketchRegisters && zeros != 0 {
		estimate = sketchRegisters*math.Log(float64(sketchRegisters)/float64(zeros))
	}
	out.Distinct = uint64(math.Round(estimate))
	if out.Distinct > out.Count { out.Distinct = out.Count }
	return out
}
