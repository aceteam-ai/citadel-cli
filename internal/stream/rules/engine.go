package rules

import (
	"container/heap"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/stream/event"
	"github.com/aceteam-ai/citadel-cli/internal/stream/ops"
)

// Options never changes process policy. All time and identity is supplied by
// the driver. An event-time driver must Flush at EOF; a live driver advances
// its watermark even when no new event arrives.
type Options struct {
	TimeMode         string
	AllowedLateness  time.Duration
	MaxPending       int
	Limits           ops.Limits
	StreamID         string
	DefinitionDigest string
}

type Evidence struct {
	Mode    string   `json:"mode"`
	Count   uint64   `json:"count"`
	Digests []string `json:"digests"`
	Samples []string `json:"samples,omitempty"`
}

// Alert is stream.alert.v1. Escalate=false is an evaluator match, not a
// request to run a cloud flow. The stream driver must never forward it.
// ST1 emits digest-only evidence. No source line is present in its JSON.
type Alert struct {
	SchemaVersion    string   `json:"schema_version"`
	AlertID          string   `json:"alert_id"`
	StreamID         string   `json:"stream_id"`
	DefinitionDigest string   `json:"definition_digest"`
	PackDigest       string   `json:"pack_digest"`
	RuleID           string   `json:"rule_id"`
	RuleVersion      string   `json:"rule_version"`
	GroupKey         string   `json:"group_key"`
	WindowStartNS    int64    `json:"window_start_ns,string"`
	WindowEndNS      int64    `json:"window_end_ns,string"`
	MatchCount       uint64   `json:"match_count"`
	Approximate      bool     `json:"approximate"`
	TMonoNS          int64    `json:"t_mono_ns,string"`
	TWallNS          int64    `json:"t_wall_ns,string"`
	ClockState       string   `json:"clock_state"`
	SourceID         string   `json:"source_id"`
	RawRefs          []string `json:"raw_refs"`
	Evidence         Evidence `json:"evidence"`
	Escalate         bool     `json:"escalate"`
}

type EngineStats struct {
	Events          uint64               `json:"events"`
	Late            uint64               `json:"late"`
	PendingRejected uint64               `json:"pending_rejected"`
	Deduplicated    uint64               `json:"deduplicated"`
	RateLimited     uint64               `json:"rate_limited"`
	StateRejected   uint64               `json:"state_rejected"`
	Matches         map[string]uint64    `json:"matches"`
	State           map[string]ops.Stats `json:"state"`
}

type ruleState struct {
	window   *ops.Window
	sequence *ops.Sequence
	dedup    *ops.Dedup
	rate     *ops.Rate
	absence  *ops.Absence
	clocks   *clockCache
}

type clockEntry struct {
	key         string
	event       event.Event
	at          int64
	fired       bool
	approximate bool
}
type clockCache struct {
	limit   int
	fields  []string
	entries map[string]*list.Element
	lru     list.List
}

func (c *clockCache) remember(key string, ev event.Event, at int64, approximate bool) {
	ev.Raw = ""
	ev.RawRef = ""
	values := map[string]any{}
	for _, k := range c.fields {
		if v, ok := ev.Fields[k]; ok {
			if s, ok := v.(string); ok {
				v = strings.Clone(s)
			}
			values[k] = v
		}
	}
	ev.Fields = values
	if old := c.entries[key]; old != nil {
		old.Value = clockEntry{key: key, event: ev, at: at, approximate: approximate}
		c.lru.MoveToBack(old)
		return
	}
	if len(c.entries) == c.limit {
		old := c.lru.Front()
		delete(c.entries, old.Value.(clockEntry).key)
		c.lru.Remove(old)
	}
	key = strings.Clone(key)
	c.entries[key] = c.lru.PushBack(clockEntry{key: key, event: ev, at: at, approximate: approximate})
}

type timerMatch struct {
	rule        *compiledRule
	missing     ops.Missing
	event       event.Event
	approximate bool
}

type pendingEvent struct {
	event event.Event
	at    int64
	order uint64
}
type pendingHeap []pendingEvent

func (p pendingHeap) Len() int { return len(p) }
func (p pendingHeap) Less(i, j int) bool {
	if p[i].at == p[j].at {
		return p[i].order < p[j].order
	}
	return p[i].at < p[j].at
}
func (p pendingHeap) Swap(i, j int) { p[i], p[j] = p[j], p[i] }
func (p *pendingHeap) Push(x any)   { *p = append(*p, x.(pendingEvent)) }
func (p *pendingHeap) Pop() any {
	a := *p
	n := len(a) - 1
	x := a[n]
	a[n] = pendingEvent{}
	*p = a[:n]
	return x
}

// Engine is a single-owner state machine. Neither evaluation nor watermark
// advancement reads a clock, creates a goroutine, or performs I/O.
type Engine struct {
	pack      *Pack
	options   Options
	states    map[string]*ruleState
	stats     EngineStats
	pending   pendingHeap
	nextOrder uint64
	maxSeen   int64
	watermark int64
	lastAt    int64
	lastEvent event.Event
	failure   error
}

func NewEngine(pack *Pack, options Options) (*Engine, error) {
	if pack == nil {
		return nil, fmt.Errorf("pack is required")
	}
	if options.TimeMode == "" {
		options.TimeMode = "ingest"
	}
	if options.TimeMode != "ingest" && options.TimeMode != "event" {
		return nil, fmt.Errorf("unsupported time mode: %s", options.TimeMode)
	}
	if options.AllowedLateness < 0 || options.AllowedLateness > time.Hour {
		return nil, fmt.Errorf("lateness must be between 0 and 1h")
	}
	if options.MaxPending == 0 {
		options.MaxPending = 10000
	}
	if options.MaxPending < 1 || options.MaxPending > 100000 {
		return nil, fmt.Errorf("pending limit must be 1 to 100000")
	}
	if options.Limits.MaxKeys < 0 || options.Limits.MaxEventsPerKey < 0 {
		return nil, fmt.Errorf("state limits must not be negative")
	}
	e := &Engine{pack: pack, options: options, states: map[string]*ruleState{}, stats: EngineStats{Matches: map[string]uint64{}, State: map[string]ops.Stats{}}}
	metadataFields := map[string]bool{}
	for _, r := range pack.rules {
		for _, k := range r.Extension.GroupBy {
			metadataFields[k] = true
		}
		if r.Correlation != nil {
			for _, k := range r.Correlation.GroupBy {
				metadataFields[k] = true
			}
		}
		if r.ValueField != "" {
			metadataFields[r.ValueField] = true
		}
	}
	fieldNames := []string{}
	for k := range metadataFields {
		fieldNames = append(fieldNames, k)
	}
	sort.Strings(fieldNames)
	for _, r := range pack.rules {
		s := &ruleState{}
		var err error
		if r.Correlation != nil {
			switch r.Correlation.Type {
			case "temporal_ordered":
				s.sequence, err = ops.NewSequence(len(r.Correlation.Rules), r.Span, options.Limits)
			default:
				if r.Extension.Window == "tumbling" {
					s.window, err = ops.NewTumblingWindow(r.Span, options.Limits)
				} else {
					s.window, err = ops.NewWindow(r.Span, options.Limits)
				}
			}
		}
		if err != nil {
			return nil, err
		}
		if r.Dedup > 0 {
			s.dedup, err = ops.NewDedup(r.Dedup, options.Limits)
			if err != nil {
				return nil, err
			}
		}
		if r.Extension.Rate != nil {
			s.rate, err = ops.NewRate(r.Extension.Rate.Capacity, r.RateEvery, options.Limits)
			if err != nil {
				return nil, err
			}
		}
		if r.Absence > 0 {
			s.absence, err = ops.NewAbsence(r.Absence, options.Limits)
			if err != nil {
				return nil, err
			}
			limit := options.Limits.MaxKeys
			if limit == 0 {
				limit = ops.DefaultMaxKeys
			}
			s.clocks = &clockCache{limit: limit, fields: fieldNames, entries: map[string]*list.Element{}}
		}
		e.states[r.ID] = s
	}
	return e, nil
}

func (e *Engine) Evaluate(ev event.Event) ([]Alert, error) {
	if e.failure != nil {
		return nil, e.failure
	}
	if ev.ParseStatus != "ok" {
		return nil, fmt.Errorf("event parse_status is not ok")
	}
	if ev.TMonoNS < 0 || ev.TWallNS < 0 {
		return nil, fmt.Errorf("negative ingest clock")
	}
	if len(ev.Fields) > 512 || len(ev.Raw) > event.HardMaxLineBytes || len(ev.SourceID) > 128 || len(ev.RawRef) > 4096 || len(ev.ClockState) > 128 {
		return nil, fmt.Errorf("event structure exceeds limits")
	}
	fields := make(map[string]any, len(ev.Fields))
	for k, v := range ev.Fields {
		if len(k) > 256 {
			return nil, fmt.Errorf("event field name exceeds limit")
		}
		if s, ok := v.(string); ok && len(s) > event.HardMaxLineBytes {
			return nil, fmt.Errorf("event field value exceeds limit")
		}
		fields[k] = v
	}
	ev.Fields = fields
	at := ev.TMonoNS
	if e.options.TimeMode == "event" {
		stamp, ok := ev.Fields["@timestamp"].(string)
		if !ok {
			return nil, fmt.Errorf("event-time mode needs @timestamp")
		}
		t, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			return nil, fmt.Errorf("invalid event @timestamp: %w", err)
		}
		if t.Before(time.Unix(0, 0)) || t.After(time.Unix(0, math.MaxInt64)) {
			return nil, fmt.Errorf("event timestamp outside nanosecond range")
		}
		at = t.UnixNano()
	}
	if at < 0 {
		return nil, fmt.Errorf("negative evaluation time")
	}
	if e.options.TimeMode == "ingest" {
		if at < e.lastAt {
			return nil, fmt.Errorf("ingest time regressed")
		}
		return e.process(ev, at)
	}
	if at < e.watermark {
		e.stats.Late++
		return []Alert{}, nil
	}
	if len(e.pending) >= e.options.MaxPending {
		e.stats.PendingRejected++
		return nil, fmt.Errorf("event-time pending queue full")
	}
	heap.Push(&e.pending, pendingEvent{ev, at, e.nextOrder})
	e.nextOrder++
	if at > e.maxSeen {
		e.maxSeen = at
	}
	watermark := e.maxSeen - int64(e.options.AllowedLateness)
	if watermark < 0 {
		watermark = 0
	}
	return e.advancePending(watermark)
}

func (e *Engine) advancePending(at int64) ([]Alert, error) {
	out := []Alert{}
	for len(e.pending) > 0 && e.pending[0].at <= at {
		p := heap.Pop(&e.pending).(pendingEvent)
		a, err := e.process(p.event, p.at)
		if err != nil {
			return nil, err
		}
		out = append(out, a...)
	}
	if at > e.watermark {
		e.watermark = at
	}
	return out, nil
}

func (e *Engine) Flush() ([]Alert, error) {
	if e.failure != nil {
		return nil, e.failure
	}
	return e.advancePending(e.maxSeen)
}

// Advance evaluates absence without manufacturing input events. In event-time
// mode at is a watermark, so callers must subtract their allowed lateness.
func (e *Engine) Advance(at int64) ([]Alert, error) {
	if e.failure != nil {
		return nil, e.failure
	}
	if at < 0 || at < e.lastAt || at < e.watermark {
		return nil, fmt.Errorf("watermark regressed")
	}
	out, err := e.advancePending(at)
	if err != nil {
		return nil, err
	}
	meta := e.lastEvent
	meta.Raw = ""
	meta.RawRef = ""
	if e.options.TimeMode == "event" {
		meta.TWallNS = at
	} else if at-e.lastAt <= math.MaxInt64-meta.TWallNS {
		meta.TWallNS += at - e.lastAt
	}
	meta.TMonoNS = at
	out = append(out, e.absences(meta, at)...)
	if e.failure != nil {
		return nil, e.failure
	}
	e.expireState(at)
	e.lastAt = at
	e.lastEvent = meta
	return out, nil
}

func (e *Engine) process(ev event.Event, at int64) ([]Alert, error) {
	if at < e.lastAt {
		return nil, fmt.Errorf("evaluation time regressed")
	}
	out := []Alert{}
	if at > e.lastAt && at > 0 {
		out = append(out, e.absences(ev, at-1)...)
	}
	if e.failure != nil {
		return nil, e.failure
	}
	e.stats.Events++
	e.lastAt = at
	e.lastEvent = ev
	matched, err := e.runRules(ev, at, nil)
	if err != nil {
		return nil, err
	}
	out = append(out, matched...)
	out = append(out, e.absences(ev, at)...)
	if e.failure != nil {
		return nil, e.failure
	}
	e.expireState(at)
	return out, nil
}

func (e *Engine) runRules(ev event.Event, at int64, seed *timerMatch) ([]Alert, error) {
	out := []Alert{}
	signals := map[string]bool{} // values retain upstream approximation
	for _, r := range e.pack.rules {
		s := e.states[r.ID]
		fields := r.Extension.GroupBy
		if r.Correlation != nil {
			fields = r.Correlation.GroupBy
		}
		key, ok := groupKey(fields, ev.Fields)
		if !ok {
			continue
		}
		matched := false
		count := uint64(1)
		start := at
		approximate := false
		seeded := seed != nil && seed.rule == r
		if seeded {
			matched = true
			key = seed.missing.Key
			start = seed.missing.LastSeen
			count = 0
			approximate = seed.approximate
		} else if r.Correlation == nil {
			matched = seed == nil && r.Test(ev.Fields)
		} else {
			refs := r.Correlation.Rules
			active := []int{}
			for i, id := range refs {
				if upstream, ok := signals[id]; ok {
					active = append(active, i)
					approximate = approximate || upstream
				}
			}
			if len(active) == 0 {
				continue
			}
			switch r.Correlation.Type {
			case "temporal_ordered":
				// Reverse step processing prevents one event from manufacturing
				// both the earlier and later member of an ordered sequence.
				for i := len(active) - 1; i >= 0; i-- {
					m := s.sequence.ObserveAnnotated(key, active[i], at, signals[refs[active[i]]])
					if m.Rejected {
						return nil, fmt.Errorf("sequence input refused")
					}
					if m.Matched {
						matched = true
						start = m.WindowStart
						count = uint64(len(refs))
						approximate = approximate || m.Approximate
					}
				}
			case "temporal":
				var snap ops.Snapshot
				for _, i := range active {
					snap = s.window.ObserveAnnotated(key, refs[i], at, signals[refs[i]])
				}
				if snap.Rejected {
					return nil, fmt.Errorf("temporal input refused")
				}
				matched = snap.Distinct == uint64(len(refs))
				count = snap.Distinct
				start = snap.WindowStart
				approximate = approximate || snap.Approximate
			default:
				value := ""
				if r.Correlation.Type == "value_count" {
					v, ok := ev.Fields[r.ValueField]
					if !ok || v == nil {
						continue
					}
					normalized, ok := canonicalValue(v)
					if !ok {
						return nil, fmt.Errorf("unsupported distinct value type")
					}
					b, err := json.Marshal(normalized)
					if err != nil {
						return nil, err
					}
					value = string(b)
				}
				snap := s.window.ObserveAnnotated(key, value, at, approximate)
				if snap.Rejected {
					return nil, fmt.Errorf("window input refused")
				}
				count = snap.Count
				if r.Correlation.Type == "value_count" {
					count = snap.Distinct
				}
				start = snap.WindowStart
				approximate = snap.Approximate
				matched = compare(count, r.Compare, r.Threshold)
			}
		}
		if !matched {
			continue
		}
		if s.absence != nil && !seeded {
			if !s.absence.Observe(key, at) {
				return nil, fmt.Errorf("absence input refused")
			}
			s.clocks.remember(key, ev, at, approximate)
			continue
		}
		e.stats.Matches[r.ID]++
		if s.dedup != nil && !s.dedup.Allow(key, at) {
			e.stats.Deduplicated++
			continue
		}
		signals[r.ID] = approximate
		if s.rate != nil && !s.rate.Allow(key, at) {
			e.stats.RateLimited++
			continue
		}
		end := at
		if seeded {
			end = seed.missing.Deadline
		}
		out = append(out, e.alert(r, key, start, end, count, approximate, ev))
	}
	return out, nil
}

func (e *Engine) absences(_ event.Event, at int64) []Alert {
	out := []Alert{}
	// Visit actual timer deadlines in order. Advancing all absence stores to
	// the poll time first would make a chained timer's earlier observation
	// regress, and would stamp alerts with an unrelated incoming event.
	for {
		next := int64(math.MaxInt64)
		found := false
		for _, r := range e.pack.rules {
			s := e.states[r.ID]
			if s.clocks == nil {
				continue
			}
			for _, entry := range s.clocks.entries {
				clock := entry.Value.(clockEntry)
				if clock.fired || clock.at > math.MaxInt64-int64(r.Absence) {
					continue
				}
				deadline := clock.at + int64(r.Absence)
				if deadline <= at && (!found || deadline < next) {
					found = true
					next = deadline
				}
			}
		}
		if !found {
			break
		}
		due := []timerMatch{}
		for _, r := range e.pack.rules {
			s := e.states[r.ID]
			if s.absence == nil {
				continue
			}
			for _, missing := range s.absence.Tick(next) {
				entry := s.clocks.entries[missing.Key]
				if entry == nil {
					e.stats.StateRejected++
					e.failure = fmt.Errorf("absence metadata unavailable")
					return nil
				}
				clock := entry.Value.(clockEntry)
				clock.fired = true
				entry.Value = clock
				ev := clock.event
				elapsed := next - clock.at
				if elapsed < 0 || ev.TMonoNS > math.MaxInt64-elapsed || ev.TWallNS > math.MaxInt64-elapsed {
					e.stats.StateRejected++
					e.failure = fmt.Errorf("absence clock overflow")
					return nil
				}
				ev.TMonoNS += elapsed
				ev.TWallNS += elapsed
				due = append(due, timerMatch{r, missing, ev, clock.approximate})
			}
		}
		sort.SliceStable(due, func(i, j int) bool {
			a, b := due[i], due[j]
			if a.rule.ID != b.rule.ID {
				return a.rule.ID < b.rule.ID
			}
			return a.missing.Key < b.missing.Key
		})
		for _, pulse := range due {
			a, err := e.runRules(pulse.event, next, &pulse)
			if err != nil {
				e.stats.StateRejected++
				e.failure = err
				return nil
			}
			out = append(out, a...)
		}
	}
	return out
}

func (e *Engine) expireState(at int64) {
	for _, s := range e.states {
		if s.window != nil {
			s.window.Advance(at)
		}
		if s.dedup != nil {
			s.dedup.Advance(at)
		}
		if s.rate != nil {
			s.rate.Advance(at)
		}
		if s.sequence != nil {
			s.sequence.Advance(at)
		}
	}
}

func (e *Engine) alert(r *compiledRule, key string, start, end int64, count uint64, approximate bool, ev event.Event) Alert {
	groupDigest := sha256.Sum256([]byte(key))
	publicKey := "sha256:" + hex.EncodeToString(groupDigest[:])
	identity, _ := json.Marshal([]any{e.pack.Digest, r.ID, r.Version, publicKey, strconv.FormatInt(start, 10)})
	digest := sha256.Sum256(identity)
	evidence := Evidence{Mode: "digest", Digests: []string{}}
	refs := []string{}
	if ev.Raw != "" {
		d := sha256.Sum256([]byte(ev.Raw))
		evidence.Count = 1
		evidence.Digests = append(evidence.Digests, "sha256:"+hex.EncodeToString(d[:]))
	}
	if ev.RawRef != "" {
		refs = append(refs, ev.RawRef)
	}
	escalate := true
	if r.Extension.Escalate != nil {
		escalate = *r.Extension.Escalate
	}
	return Alert{SchemaVersion: "stream.alert.v1", AlertID: hex.EncodeToString(digest[:]), StreamID: e.options.StreamID, DefinitionDigest: e.options.DefinitionDigest, PackDigest: e.pack.Digest, RuleID: r.ID, RuleVersion: r.Version, GroupKey: publicKey, WindowStartNS: start, WindowEndNS: end, MatchCount: count, Approximate: approximate, TMonoNS: ev.TMonoNS, TWallNS: ev.TWallNS, ClockState: ev.ClockState, SourceID: ev.SourceID, RawRefs: refs, Evidence: evidence, Escalate: escalate}
}

func groupKey(fields []string, values map[string]any) (string, bool) {
	if len(fields) == 0 {
		return "[]", true
	}
	parts := make([]any, 0, len(fields)*2)
	for _, name := range fields {
		v, ok := values[name]
		if !ok || v == nil {
			return "", false
		}
		v, ok = canonicalValue(v)
		if !ok {
			return "", false
		}
		parts = append(parts, name, v)
	}
	b, err := json.Marshal(parts)
	if err != nil || len(b) > ops.MaxKeyBytes {
		return "", false
	}
	return string(b), true
}

func compare(value uint64, operator string, threshold uint64) bool {
	switch operator {
	case "eq":
		return value == threshold
	case "gt":
		return value > threshold
	case "gte":
		return value >= threshold
	case "lt":
		return value < threshold
	case "lte":
		return value <= threshold
	}
	return false
}

func (e *Engine) Stats() EngineStats {
	s := e.stats
	s.Matches = map[string]uint64{}
	s.State = map[string]ops.Stats{}
	for id, n := range e.stats.Matches {
		s.Matches[id] = n
	}
	for id, state := range e.states {
		items := []ops.Stats{}
		if state.window != nil {
			items = append(items, state.window.Stats())
		}
		if state.sequence != nil {
			items = append(items, state.sequence.Stats())
		}
		if state.dedup != nil {
			items = append(items, state.dedup.Stats())
		}
		if state.rate != nil {
			items = append(items, state.rate.Stats())
		}
		if state.absence != nil {
			items = append(items, state.absence.Stats())
		}
		var total ops.Stats
		for _, v := range items {
			total.StateEvicted += v.StateEvicted
			total.KeyEvictions += v.KeyEvictions
			total.EventEvictions += v.EventEvictions
			total.InputRejected += v.InputRejected
			total.ApproximateTransitions += v.ApproximateTransitions
			total.ActiveKeys += v.ActiveKeys
		}
		s.State[id] = total
	}
	return s
}

// A typed rational tag gives all exact numeric spellings one local identity,
// without ever aliasing the string "1" with the number 1. JSON decimal and
// exponent spelling must not evade grouping or inflate distinct counts.
func canonicalValue(value any) (any, bool) {
	switch v := value.(type) {
	case string:
		return v, true
	case bool:
		return v, true
	}
	if n, ok := detectionNumber(value); ok {
		return map[string]string{"$number": n.RatString()}, true
	}
	return nil, false
}

// SortAlerts orders independently produced collections for canonical reports.
func SortAlerts(alerts []Alert) {
	sort.SliceStable(alerts, func(i, j int) bool {
		a, b := alerts[i], alerts[j]
		if a.WindowEndNS != b.WindowEndNS {
			return a.WindowEndNS < b.WindowEndNS
		}
		if a.RuleID != b.RuleID {
			return a.RuleID < b.RuleID
		}
		if a.GroupKey != b.GroupKey {
			return a.GroupKey < b.GroupKey
		}
		return a.AlertID < b.AlertID
	})
}
