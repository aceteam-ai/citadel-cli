// Package ops contains bounded, deterministic stream state primitives. Callers
// supply nonnegative monotonic nanoseconds; the package never reads a clock.
// Instances are single-owner objects, not safe for concurrent use.
package ops

import (
	"container/list"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	DefaultMaxKeys         = 100000
	DefaultMaxEventsPerKey = 4096
	MaxKeyBytes            = 4096
	MaxValueBytes          = 65536
)

// Limits bounds retained keys and observations, not merely an output page.
// A zero field selects its default; negative values are refused.
type Limits struct {
	MaxKeys         int
	MaxEventsPerKey int
}

// Stats exposes every capacity-driven loss as StateEvicted. Normal expiry is
// not a loss. EventEvictions counts individual observations and KeyEvictions
// counts entire key states, so StateEvicted is their sum.
type Stats struct {
	StateEvicted           uint64
	KeyEvictions           uint64
	EventEvictions         uint64
	InputRejected          uint64
	ApproximateTransitions uint64
	ActiveKeys             int
}

type keyed[T any] struct {
	key   string
	last  int64
	value T
}

type cache[T any] struct {
	limits  Limits
	entries map[string]*list.Element
	lru     list.List
	stats   Stats
	now     int64
}

func newCache[T any](limits Limits) (*cache[T], error) {
	if limits.MaxKeys < 0 || limits.MaxEventsPerKey < 0 {
		return nil, errors.New("stream state limits must not be negative")
	}
	if limits.MaxKeys == 0 {
		limits.MaxKeys = DefaultMaxKeys
	}
	if limits.MaxEventsPerKey == 0 {
		limits.MaxEventsPerKey = DefaultMaxEventsPerKey
	}
	return &cache[T]{limits: limits, entries: make(map[string]*list.Element)}, nil
}

func (c *cache[T]) accept(key string, at int64) bool {
	if len(key) > MaxKeyBytes || at < 0 || at < c.now {
		c.stats.InputRejected++
		return false
	}
	c.now = at
	return true
}

func (c *cache[T]) advance(at int64) bool {
	return c.accept("", at)
}

func (c *cache[T]) get(key string) (*keyed[T], bool) {
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	return e.Value.(*keyed[T]), true
}

func (c *cache[T]) touch(key string, at int64, initial T) *keyed[T] {
	if e, ok := c.entries[key]; ok {
		v := e.Value.(*keyed[T])
		v.last = at
		c.lru.MoveToBack(e)
		return v
	}
	if len(c.entries) == c.limits.MaxKeys {
		c.remove(c.lru.Front())
		c.stats.KeyEvictions++
		c.stats.StateEvicted++
	}
	// A short substring may otherwise retain a much larger source line buffer.
	retainedKey := strings.Clone(key)
	v := &keyed[T]{key: retainedKey, last: at, value: initial}
	c.entries[retainedKey] = c.lru.PushBack(v)
	return v
}

func (c *cache[T]) remove(e *list.Element) {
	delete(c.entries, e.Value.(*keyed[T]).key)
	c.lru.Remove(e)
}

// expire removes states whose last observation is outside (at-span, at].
// Supplied time is monotonic, so the LRU is also ordered by last observation.
func (c *cache[T]) expire(at, span int64) {
	for e := c.lru.Front(); e != nil; e = c.lru.Front() {
		if e.Value.(*keyed[T]).last > at-span {
			break
		}
		c.remove(e)
	}
}

func (c *cache[T]) snapshot() Stats {
	s := c.stats
	s.ActiveKeys = len(c.entries)
	return s
}

func validSpan(span time.Duration) error {
	if span <= 0 {
		return errors.New("stream state span must be positive")
	}
	return nil
}

func deadline(at, span int64) int64 {
	if at > math.MaxInt64-span {
		return math.MaxInt64
	}
	return at + span
}

// Dedup admits a key once per fixed window anchored to its last admission.
// Rejected duplicates update LRU recency but do not extend suppression.
type Dedup struct {
	state *cache[int64]
	span  int64
}

func NewDedup(span time.Duration, limits Limits) (*Dedup, error) {
	if err := validSpan(span); err != nil {
		return nil, err
	}
	s, err := newCache[int64](limits)
	if err != nil {
		return nil, err
	}
	return &Dedup{state: s, span: int64(span)}, nil
}

func (d *Dedup) Allow(key string, at int64) bool {
	if !d.state.accept(key, at) {
		return false
	}
	d.state.expire(at, d.span)
	v, ok := d.state.get(key)
	if ok && at-v.value < d.span {
		d.state.touch(key, at, 0)
		return false
	}
	d.state.touch(key, at, at).value = at
	return true
}

func (d *Dedup) Stats() Stats { return d.state.snapshot() }
func (d *Dedup) Advance(at int64) {
	if d.state.advance(at) {
		d.state.expire(at, d.span)
	}
}

type bucket struct {
	tokens   uint64
	refilled int64
}

// Rate is an integer token bucket: a new key starts at capacity, each Allow
// consumes one token, and each full refill interval restores one token.
type Rate struct {
	state    *cache[bucket]
	capacity uint64
	interval int64
	idleSpan int64
}

func NewRate(capacity uint64, refillEvery time.Duration, limits Limits) (*Rate, error) {
	if capacity == 0 || refillEvery <= 0 || capacity > uint64(math.MaxInt64/int64(refillEvery)) {
		return nil, errors.New("stream rate capacity and refill interval must be positive and bounded")
	}
	s, err := newCache[bucket](limits)
	if err != nil {
		return nil, err
	}
	return &Rate{state: s, capacity: capacity, interval: int64(refillEvery), idleSpan: int64(capacity) * int64(refillEvery)}, nil
}

func (r *Rate) Allow(key string, at int64) bool {
	if !r.state.accept(key, at) {
		return false
	}
	r.state.expire(at, r.idleSpan)
	v := r.state.touch(key, at, bucket{tokens: r.capacity, refilled: at})
	steps := uint64((at - v.value.refilled) / r.interval)
	if steps > 0 {
		if steps >= r.capacity-v.value.tokens {
			v.value.tokens = r.capacity
		} else {
			v.value.tokens += steps
		}
		v.value.refilled += int64(steps) * r.interval
	}
	if v.value.tokens == 0 {
		return false
	}
	v.value.tokens--
	return true
}

func (r *Rate) Stats() Stats { return r.state.snapshot() }
func (r *Rate) Advance(at int64) {
	if r.state.advance(at) {
		r.state.expire(at, r.idleSpan)
	}
}

type absent struct {
	seen    int64
	emitted bool
}

// Missing is emitted once per silence episode. Observe starts a new episode.
type Missing struct {
	Key      string
	LastSeen int64
	Deadline int64
}

type Absence struct {
	state *cache[absent]
	span  int64
}

func NewAbsence(span time.Duration, limits Limits) (*Absence, error) {
	if err := validSpan(span); err != nil {
		return nil, err
	}
	s, err := newCache[absent](limits)
	if err != nil {
		return nil, err
	}
	return &Absence{state: s, span: int64(span)}, nil
}

func (a *Absence) Observe(key string, at int64) bool {
	if !a.state.accept(key, at) {
		return false
	}
	a.state.touch(key, at, absent{}).value = absent{seen: at}
	return true
}

func (a *Absence) Tick(at int64) []Missing {
	if !a.state.advance(at) {
		return nil
	}
	var out []Missing
	for _, e := range a.state.entries {
		v := e.Value.(*keyed[absent])
		if !v.value.emitted && at-v.value.seen >= a.span {
			out = append(out, Missing{Key: v.key, LastSeen: v.value.seen, Deadline: deadline(v.value.seen, a.span)})
			v.value.emitted = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (a *Absence) Stats() Stats { return a.state.snapshot() }

type progress struct {
	starts      []int64
	orders      []uint64
	valid       []bool
	approximate []bool
}

type SequenceMatch struct {
	Matched     bool
	Rejected    bool
	WindowStart int64
	Approximate bool
}

// Sequence keeps the latest start for each partial stage. A newer first step
// must not erase an older candidate that has already reached a later stage:
// A1 B2 A3 C4 completes A1 B2 C4. For existence matching, the latest start at a
// particular stage dominates earlier starts at that same stage. Completion
// consumes candidates no newer than its start, retaining newer partials.
// The span is inclusive at both ends; each step needs a separate Observe call
// (ties keep input order). Retained per-key state is bounded by the step count.
type Sequence struct {
	state *cache[progress]
	steps int
	span  int64
	order uint64
}

func NewSequence(steps int, span time.Duration, limits Limits) (*Sequence, error) {
	if steps < 2 || steps > 64 {
		return nil, errors.New("stream sequence must have between 2 and 64 steps")
	}
	if err := validSpan(span); err != nil {
		return nil, err
	}
	s, err := newCache[progress](limits)
	if err != nil {
		return nil, err
	}
	return &Sequence{state: s, steps: steps, span: int64(span)}, nil
}

func (s *Sequence) Observe(key string, step int, at int64) SequenceMatch {
	return s.ObserveAnnotated(key, step, at, false)
}

func (s *Sequence) ObserveAnnotated(key string, step int, at int64, approximate bool) SequenceMatch {
	if step < 0 || step >= s.steps {
		s.state.stats.InputRejected++
		return SequenceMatch{Rejected: true}
	}
	if s.order == math.MaxUint64 {
		s.state.stats.InputRejected++
		return SequenceMatch{Rejected: true}
	}
	if !s.state.accept(key, at) {
		return SequenceMatch{Rejected: true}
	}
	s.order++
	// Strict expiry preserves an event exactly at the sequence deadline.
	if at > s.span {
		s.state.expire(at-1, s.span)
	}
	v, ok := s.state.get(key)
	if !ok {
		if step != 0 {
			return SequenceMatch{}
		}
		v = s.state.touch(key, at, progress{starts: make([]int64, s.steps-1), orders: make([]uint64, s.steps-1), valid: make([]bool, s.steps-1), approximate: make([]bool, s.steps-1)})
	}
	s.state.touch(key, at, progress{})
	for i := range v.value.valid {
		if v.value.valid[i] && at-v.value.starts[i] > s.span {
			v.value.valid[i] = false
		}
	}
	if step == 0 {
		v.value.starts[0] = at
		v.value.orders[0] = s.order
		v.value.valid[0] = true
		v.value.approximate[0] = approximate
		return SequenceMatch{}
	}
	if !v.value.valid[step-1] {
		return SequenceMatch{}
	}
	start := v.value.starts[step-1]
	order := v.value.orders[step-1]
	approximate = approximate || v.value.approximate[step-1]
	if step < s.steps-1 {
		v.value.starts[step] = start
		v.value.orders[step] = order
		v.value.valid[step] = true
		v.value.approximate[step] = approximate
		return SequenceMatch{}
	}
	for i := range v.value.valid {
		if v.value.valid[i] && v.value.orders[i] <= order {
			v.value.valid[i] = false
		}
	}
	active := false
	for _, valid := range v.value.valid {
		active = active || valid
	}
	if !active {
		s.state.remove(s.state.entries[key])
	}
	return SequenceMatch{Matched: true, WindowStart: start, Approximate: approximate}
}

func (s *Sequence) Stats() Stats { return s.state.snapshot() }

func (s *Sequence) Advance(at int64) {
	if !s.state.advance(at) {
		return
	}
	for key, entry := range s.state.entries {
		v := entry.Value.(*keyed[progress])
		active := false
		for i, valid := range v.value.valid {
			if valid && at-v.value.starts[i] > s.span {
				v.value.valid[i] = false
			}
			active = active || v.value.valid[i]
		}
		if !active {
			s.state.remove(s.state.entries[key])
		}
	}
}
