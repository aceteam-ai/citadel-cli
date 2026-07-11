package status

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultIdleThresholdSeconds is the number of seconds a managed service may go
// without serving a request before it is reported as idle. Overridable via the
// SERVICE_IDLE_THRESHOLD_SECONDS environment variable.
const DefaultIdleThresholdSeconds = 300

// IdleThresholdSeconds returns the configured idle threshold. It reads
// SERVICE_IDLE_THRESHOLD_SECONDS and falls back to DefaultIdleThresholdSeconds
// when unset, empty, non-numeric, or non-positive.
func IdleThresholdSeconds() int {
	if v := os.Getenv("SERVICE_IDLE_THRESHOLD_SECONDS"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n
		}
	}
	return DefaultIdleThresholdSeconds
}

// IdleState is the per-service idle signal surfaced in the heartbeat. It answers
// the "busy but idle" question the platform scheduler needs to reclaim VRAM: a
// service can be up and holding GPU memory while having served no requests for
// a long time.
//
// LastRequestAt is the wall-clock time of the most recent observed request, or
// nil when no request has ever been seen since tracking began. IdleSeconds is
// the number of seconds since LastRequestAt (or since tracking began, if no
// request was ever seen). Idle is true when IdleSeconds meets or exceeds the
// configured threshold.
type IdleState struct {
	Idle          bool       `json:"idle"`
	IdleSeconds   int64      `json:"idle_seconds"`
	LastRequestAt *time.Time `json:"last_request_at,omitempty"`
}

// engineMetrics holds the request signals scraped from an engine's Prometheus
// /metrics endpoint. total is a monotonic count of completed requests; active
// is the number of requests running or queued right now.
type engineMetrics struct {
	total    float64
	active   float64
	hasTotal bool
}

// idleEntry is the tracked state for a single (engine, port) endpoint.
type idleEntry struct {
	startedAt     time.Time
	lastTotal     float64
	haveLastTotal bool
	lastRequestAt time.Time
	haveRequest   bool
}

// IdleTracker scrapes managed-engine metrics endpoints and derives a per-service
// idle signal. It is safe for concurrent use.
//
// Signal source and limitation: the tracker prefers the engine's own Prometheus
// /metrics endpoint (vLLM exposes request counters and running/waiting gauges).
// The citadel gateway is NOT in the general inference path — only /v1/embeddings
// is proxied through it — so gateway-counted requests are not a reliable signal
// for chat/completions services; scraping the engine directly is the cheapest
// reliable signal. Because tracking state lives in-process, idle_seconds is
// measured since citadel began tracking a service: a freshly restarted citadel
// cannot prove a service was idle before startup, and the first scrape has no
// prior counter baseline (it seeds last_request_at to the tracking start time).
type IdleTracker struct {
	client    *http.Client
	threshold time.Duration
	now       func() time.Time

	mu      sync.Mutex
	entries map[string]*idleEntry
}

// NewIdleTracker creates an IdleTracker with the given idle threshold (in
// seconds). A threshold <= 0 falls back to the configured default.
func NewIdleTracker(thresholdSeconds int) *IdleTracker {
	if thresholdSeconds <= 0 {
		thresholdSeconds = IdleThresholdSeconds()
	}
	return &IdleTracker{
		client:    &http.Client{Timeout: 3 * time.Second},
		threshold: time.Duration(thresholdSeconds) * time.Second,
		now:       time.Now,
		entries:   make(map[string]*idleEntry),
	}
}

// Observe scrapes the engine's metrics endpoint on the given port and returns
// the updated idle state. engineType selects the metrics dialect ("vllm").
// key uniquely identifies the service instance (typically its name) so state
// persists across calls even if the port is reused. Returns ok=false when no
// usable signal is available (unknown engine or the metrics endpoint could not
// be scraped) — callers should then omit the idle fields rather than report a
// misleading "idle since startup".
func (t *IdleTracker) Observe(ctx context.Context, key, engineType string, port int) (IdleState, bool) {
	metrics, err := t.scrape(ctx, engineType, port)
	if err != nil || !metrics.hasTotal {
		return IdleState{}, false
	}
	return t.record(key, metrics), true
}

// record folds a fresh metrics sample into the tracked state for key and
// computes the resulting idle state. Exposed logic is unit-tested directly via
// RecordSample so no live engine is needed.
func (t *IdleTracker) record(key string, m engineMetrics) IdleState {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	e, ok := t.entries[key]
	if !ok {
		// First observation of this service: seed the baseline. We cannot know
		// when the last request happened before we started tracking, so treat
		// "tracking start" as the reference point for idle_seconds.
		e = &idleEntry{startedAt: now, lastRequestAt: now}
		t.entries[key] = e
	}

	// A request is "active right now" if the engine reports running or queued
	// work; a completed request shows up as an increase in the monotonic total.
	activeNow := m.active > 0
	advanced := e.haveLastTotal && m.total > e.lastTotal
	// Guard against a counter reset (engine restart): total dropping below the
	// last seen value is not a real request, so do not refresh on it.
	if activeNow || advanced {
		e.lastRequestAt = now
		e.haveRequest = e.haveRequest || advanced || activeNow
	}

	e.lastTotal = m.total
	e.haveLastTotal = true

	return t.stateLocked(e, now)
}

// RecordSample is a test-friendly wrapper around record that folds a raw
// (total, active) sample for key and returns the resulting idle state.
func (t *IdleTracker) RecordSample(key string, total, active float64, hasTotal bool) IdleState {
	return t.record(key, engineMetrics{total: total, active: active, hasTotal: hasTotal})
}

// RecordActivityCounter folds a monotonic activity counter (e.g. a container's
// cumulative network I/O in bytes) into the tracked idle state for key. It is
// the generic, request-agnostic idle path for services that expose no
// vLLM-style request metrics (Claude Code, OpenClaw, Hermes, ...): any increase
// in the counter since the last sample is treated as a request, refreshing
// last_request_at; a flat counter accumulates idle_seconds until the configured
// threshold flips Idle=true. See citadel-cli#433.
//
// Reset semantics differ deliberately from the vLLM request-counter path. For
// vLLM, a counter DROP means an engine restart with the model already resident,
// so record() does NOT refresh last_request_at (the drop is not a real request).
// For a container network counter, a DROP means the container itself just
// restarted, which is fresh activity (a brand-new, busy container). Treating
// that as idle would let a just-restarted service be auto-stopped immediately on
// a stale idle age. So a drop here refreshes last_request_at: restart == active.
func (t *IdleTracker) RecordActivityCounter(key string, counter uint64) IdleState {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	e, ok := t.entries[key]
	if !ok {
		// First observation: seed the baseline. We cannot know when the last
		// activity happened before tracking began, so treat "now" as the
		// reference point for idle_seconds and prove no request yet.
		e = &idleEntry{startedAt: now, lastRequestAt: now}
		t.entries[key] = e
	}

	c := float64(counter)
	// Any change from the prior sample is activity: an increase is new traffic,
	// a decrease means the container restarted (its counter reset) which is
	// itself fresh activity. Only a byte-for-byte identical counter is "quiet".
	changed := e.haveLastTotal && c != e.lastTotal
	if changed {
		e.lastRequestAt = now
		e.haveRequest = true
	}
	e.lastTotal = c
	e.haveLastTotal = true

	return t.stateLocked(e, now)
}

// stateLocked computes the IdleState for an entry. Caller must hold t.mu.
func (t *IdleTracker) stateLocked(e *idleEntry, now time.Time) IdleState {
	idleFor := now.Sub(e.lastRequestAt)
	if idleFor < 0 {
		idleFor = 0
	}
	st := IdleState{
		Idle:        idleFor >= t.threshold,
		IdleSeconds: int64(idleFor.Seconds()),
	}
	if e.haveRequest {
		last := e.lastRequestAt
		st.LastRequestAt = &last
	}
	return st
}

// scrape fetches and parses the engine's metrics endpoint.
func (t *IdleTracker) scrape(ctx context.Context, engineType string, port int) (engineMetrics, error) {
	dialect, ok := metricsDialects[strings.ToLower(engineType)]
	if !ok {
		return engineMetrics{}, fmt.Errorf("no idle metrics dialect for engine %q", engineType)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, dialect.path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return engineMetrics{}, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return engineMetrics{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return engineMetrics{}, fmt.Errorf("metrics endpoint returned %d", resp.StatusCode)
	}
	return parsePrometheusMetrics(resp.Body, dialect), nil
}

// metricsDialect names the Prometheus series that carry the request signals for
// a given engine. totalMetrics are monotonic completed-request counters (any one
// present is used); activeMetrics are running/queued gauges summed together.
type metricsDialect struct {
	path          string
	totalMetrics  []string
	activeMetrics []string
}

// metricsDialects maps an engine type to its /metrics series names.
//
// vLLM (OpenAI server) exposes Prometheus metrics at /metrics. Metric names have
// drifted across versions; we accept several known spellings and use whichever
// is present:
//   - completed-request counters: vllm:request_success_total (newer) and the
//     labelled request-latency count series vllm:e2e_request_latency_seconds_count.
//   - live-work gauges: vllm:num_requests_running and vllm:num_requests_waiting.
var metricsDialects = map[string]metricsDialect{
	"vllm": {
		path: "/metrics",
		totalMetrics: []string{
			"vllm:request_success_total",
			"vllm:e2e_request_latency_seconds_count",
		},
		activeMetrics: []string{
			"vllm:num_requests_running",
			"vllm:num_requests_waiting",
		},
	},
}

// parsePrometheusMetrics reads a Prometheus text-format exposition and extracts
// the total and active signals named by the dialect. Series are matched on the
// bare metric name (label sets are ignored); values for the same metric name are
// summed, which is correct both for per-label counter fan-out and for the two
// distinct active gauges.
func parsePrometheusMetrics(r io.Reader, d metricsDialect) engineMetrics {
	totalSet := make(map[string]struct{}, len(d.totalMetrics))
	for _, m := range d.totalMetrics {
		totalSet[m] = struct{}{}
	}
	activeSet := make(map[string]struct{}, len(d.activeMetrics))
	for _, m := range d.activeMetrics {
		activeSet[m] = struct{}{}
	}

	var out engineMetrics
	// Track which total metric names we actually saw so a labelled counter with
	// multiple series is summed but the presence flag stays accurate.
	seenTotal := false

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := parseMetricLine(line)
		if !ok {
			continue
		}
		if _, ok := totalSet[name]; ok {
			out.total += value
			seenTotal = true
			continue
		}
		if _, ok := activeSet[name]; ok {
			out.active += value
		}
	}
	out.hasTotal = seenTotal
	return out
}

// parseMetricLine splits one Prometheus exposition line into the bare metric
// name (label set stripped) and its float value.
func parseMetricLine(line string) (name string, value float64, ok bool) {
	// A sample line is: metric_name{labels} value [timestamp]
	// Split off the value: it is the last whitespace-delimited field before an
	// optional timestamp. Prometheus text format uses a single space between the
	// series and the value, so splitting on the last space of the "series value"
	// portion is robust enough here.
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", 0, false
	}
	// The metric series is everything up to the value; the value is the field
	// immediately after the series token. Because label sets never contain
	// unescaped spaces in practice for these engines, fields[0] is the series
	// and fields[1] is the value.
	series := fields[0]
	v, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return "", 0, false
	}
	name = series
	if i := strings.IndexByte(series, '{'); i >= 0 {
		name = series[:i]
	}
	return name, v, true
}
