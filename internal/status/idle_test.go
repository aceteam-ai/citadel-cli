package status

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTestTracker returns an IdleTracker with a controllable clock so idle_seconds
// is deterministic without sleeping.
func newTestTracker(thresholdSeconds int, clock *time.Time) *IdleTracker {
	t := NewIdleTracker(thresholdSeconds)
	t.now = func() time.Time { return *clock }
	return t
}

func TestIdleThresholdSeconds_Default(t *testing.T) {
	t.Setenv("SERVICE_IDLE_THRESHOLD_SECONDS", "")
	if got := IdleThresholdSeconds(); got != DefaultIdleThresholdSeconds {
		t.Fatalf("expected default %d, got %d", DefaultIdleThresholdSeconds, got)
	}
}

func TestIdleThresholdSeconds_EnvOverride(t *testing.T) {
	t.Setenv("SERVICE_IDLE_THRESHOLD_SECONDS", "42")
	if got := IdleThresholdSeconds(); got != 42 {
		t.Fatalf("expected 42, got %d", got)
	}
}

func TestIdleThresholdSeconds_InvalidFallsBack(t *testing.T) {
	for _, v := range []string{"0", "-5", "abc", "  "} {
		t.Setenv("SERVICE_IDLE_THRESHOLD_SECONDS", v)
		if got := IdleThresholdSeconds(); got != DefaultIdleThresholdSeconds {
			t.Fatalf("value %q: expected default %d, got %d", v, DefaultIdleThresholdSeconds, got)
		}
	}
}

func TestIdleTracker_FirstSampleSeedsBaseline(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tr := newTestTracker(300, &now)

	// First observation: no prior counter baseline. We just started tracking,
	// so idle_seconds is 0 and last_request_at is unknown (no request proven).
	st := tr.RecordSample("vllm", 5, 0, true)
	if st.Idle {
		t.Fatalf("expected not idle on first sample")
	}
	if st.IdleSeconds != 0 {
		t.Fatalf("expected idle_seconds 0 on first sample, got %d", st.IdleSeconds)
	}
	if st.LastRequestAt != nil {
		t.Fatalf("expected no last_request_at before any request is proven, got %v", st.LastRequestAt)
	}
}

func TestIdleTracker_CounterAdvanceRefreshesLastRequest(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tr := newTestTracker(300, &now)

	tr.RecordSample("vllm", 10, 0, true) // baseline

	// 60s later, the completed-request counter advanced -> a request happened.
	now = now.Add(60 * time.Second)
	st := tr.RecordSample("vllm", 11, 0, true)
	if st.LastRequestAt == nil {
		t.Fatalf("expected last_request_at to be set after counter advanced")
	}
	if !st.LastRequestAt.Equal(now) {
		t.Fatalf("expected last_request_at %v, got %v", now, *st.LastRequestAt)
	}
	if st.IdleSeconds != 0 {
		t.Fatalf("expected idle_seconds 0 right after a request, got %d", st.IdleSeconds)
	}
	if st.Idle {
		t.Fatalf("expected not idle right after a request")
	}
}

func TestIdleTracker_GoesIdleAfterThreshold(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tr := newTestTracker(300, &now)

	tr.RecordSample("vllm", 10, 0, true) // baseline
	now = now.Add(30 * time.Second)
	tr.RecordSample("vllm", 11, 0, true) // a request at t=30s

	// 299s after the last request: still under the 300s threshold.
	now = now.Add(299 * time.Second)
	st := tr.RecordSample("vllm", 11, 0, true)
	if st.Idle {
		t.Fatalf("expected not idle at 299s (< threshold)")
	}
	if st.IdleSeconds != 299 {
		t.Fatalf("expected idle_seconds 299, got %d", st.IdleSeconds)
	}

	// 1 more second: exactly at the threshold -> idle.
	now = now.Add(1 * time.Second)
	st = tr.RecordSample("vllm", 11, 0, true)
	if !st.Idle {
		t.Fatalf("expected idle at 300s (>= threshold), idle_seconds=%d", st.IdleSeconds)
	}
	if st.IdleSeconds != 300 {
		t.Fatalf("expected idle_seconds 300, got %d", st.IdleSeconds)
	}
}

func TestIdleTracker_ActiveGaugeKeepsBusy(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tr := newTestTracker(300, &now)

	tr.RecordSample("vllm", 10, 0, true) // baseline, idle
	now = now.Add(1000 * time.Second)

	// Counter did NOT advance, but a request is running right now (active gauge
	// > 0). This must be treated as busy, not idle.
	st := tr.RecordSample("vllm", 10, 2, true)
	if st.Idle {
		t.Fatalf("expected not idle while a request is actively running")
	}
	if st.IdleSeconds != 0 {
		t.Fatalf("expected idle_seconds 0 while active, got %d", st.IdleSeconds)
	}
	if st.LastRequestAt == nil || !st.LastRequestAt.Equal(now) {
		t.Fatalf("expected last_request_at %v, got %v", now, st.LastRequestAt)
	}
}

func TestIdleTracker_CounterResetDoesNotFalsePositive(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tr := newTestTracker(300, &now)

	tr.RecordSample("vllm", 100, 0, true) // baseline high
	now = now.Add(10 * time.Second)

	// Engine restarted: counter reset to a lower value. That is NOT a request;
	// last_request_at must not refresh, and no phantom request is recorded.
	st := tr.RecordSample("vllm", 3, 0, true)
	if st.LastRequestAt != nil {
		t.Fatalf("expected no last_request_at after a counter reset with no prior request, got %v", st.LastRequestAt)
	}
	if st.IdleSeconds != 10 {
		t.Fatalf("expected idle_seconds measured from tracking start (10), got %d", st.IdleSeconds)
	}
}

func TestIdleTracker_SeparateKeysTrackedIndependently(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tr := newTestTracker(300, &now)

	tr.RecordSample("a", 10, 0, true)
	tr.RecordSample("b", 10, 0, true)

	now = now.Add(60 * time.Second)
	tr.RecordSample("a", 11, 0, true) // a served a request
	stB := tr.RecordSample("b", 10, 0, true)

	if stB.IdleSeconds != 60 {
		t.Fatalf("expected b idle_seconds 60 (independent of a), got %d", stB.IdleSeconds)
	}

	now = now.Add(60 * time.Second)
	stA := tr.RecordSample("a", 11, 0, true)
	if stA.IdleSeconds != 60 {
		t.Fatalf("expected a idle_seconds 60 since its last request, got %d", stA.IdleSeconds)
	}
}

func TestRecordActivityCounter_FirstSampleSeedsBaseline(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tr := newTestTracker(300, &now)

	// First observation of a generic (non-vLLM) service: unknown history, so
	// idle_seconds is 0 and no request is proven yet. This is the fail-safe seed:
	// a service we just started tracking is never immediately "idle".
	st := tr.RecordActivityCounter("claude-code", 1000)
	if st.Idle {
		t.Fatalf("expected not idle on first activity sample")
	}
	if st.IdleSeconds != 0 {
		t.Fatalf("expected idle_seconds 0 on first sample, got %d", st.IdleSeconds)
	}
	if st.LastRequestAt != nil {
		t.Fatalf("expected no last_request_at before any activity is proven, got %v", st.LastRequestAt)
	}
}

func TestRecordActivityCounter_AdvanceResetsIdleClock(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tr := newTestTracker(300, &now)

	tr.RecordActivityCounter("openclaw", 1000) // baseline
	now = now.Add(200 * time.Second)
	// Net bytes climbed: the container did work, so the idle clock resets and
	// last_request_at is now.
	st := tr.RecordActivityCounter("openclaw", 5000)
	if st.Idle {
		t.Fatalf("expected not idle right after network activity")
	}
	if st.IdleSeconds != 0 {
		t.Fatalf("expected idle_seconds 0 after activity, got %d", st.IdleSeconds)
	}
	if st.LastRequestAt == nil || !st.LastRequestAt.Equal(now) {
		t.Fatalf("expected last_request_at %v after activity, got %v", now, st.LastRequestAt)
	}
}

func TestRecordActivityCounter_GoesIdleAfterThreshold(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tr := newTestTracker(300, &now)

	tr.RecordActivityCounter("hermes", 1000) // baseline at t=0
	now = now.Add(30 * time.Second)
	tr.RecordActivityCounter("hermes", 2000) // activity at t=30s

	// 299s of a flat counter: still under the 300s threshold.
	now = now.Add(299 * time.Second)
	st := tr.RecordActivityCounter("hermes", 2000)
	if st.Idle {
		t.Fatalf("expected not idle at 299s (< threshold)")
	}
	if st.IdleSeconds != 299 {
		t.Fatalf("expected idle_seconds 299, got %d", st.IdleSeconds)
	}

	// One more second at a flat counter: exactly at threshold -> idle.
	now = now.Add(1 * time.Second)
	st = tr.RecordActivityCounter("hermes", 2000)
	if !st.Idle {
		t.Fatalf("expected idle at 300s (>= threshold), idle_seconds=%d", st.IdleSeconds)
	}
	if st.IdleSeconds != 300 {
		t.Fatalf("expected idle_seconds 300, got %d", st.IdleSeconds)
	}
}

func TestRecordActivityCounter_RestartResetIsActiveNotIdle(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tr := newTestTracker(300, &now)

	tr.RecordActivityCounter("agent", 1_000_000) // baseline: a long-lived container
	now = now.Add(1000 * time.Second)            // long idle gap

	// The container restarted: its cumulative NetIO counter dropped to a small
	// value. Unlike the vLLM request-counter path (where a reset is NOT a
	// request), a network-counter reset means the container itself is brand new
	// and busy. It MUST reset the idle clock, otherwise a just-restarted service
	// would report a stale multi-minute idle age and be auto-stopped immediately.
	st := tr.RecordActivityCounter("agent", 500)
	if st.Idle {
		t.Fatalf("expected a just-restarted container to be active, not idle")
	}
	if st.IdleSeconds != 0 {
		t.Fatalf("expected idle_seconds 0 after a restart, got %d", st.IdleSeconds)
	}
	if st.LastRequestAt == nil || !st.LastRequestAt.Equal(now) {
		t.Fatalf("expected last_request_at %v after restart, got %v", now, st.LastRequestAt)
	}
}

func TestRecordActivityCounter_FlatFromStartGoesIdle(t *testing.T) {
	// A service that never transmits after tracking begins accumulates idle from
	// the tracking-start baseline and eventually flips idle -- this is the
	// intended auto-stop trigger for a genuinely-quiet agent app.
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tr := newTestTracker(300, &now)

	tr.RecordActivityCounter("idle-agent", 42) // baseline
	now = now.Add(300 * time.Second)
	st := tr.RecordActivityCounter("idle-agent", 42) // still 42: no traffic at all
	if !st.Idle {
		t.Fatalf("expected idle after threshold with a flat counter, idle_seconds=%d", st.IdleSeconds)
	}
	// No request was ever proven, so last_request_at stays absent.
	if st.LastRequestAt != nil {
		t.Fatalf("expected no last_request_at when no activity was ever seen, got %v", st.LastRequestAt)
	}
}

func TestParsePrometheusMetrics_VLLM(t *testing.T) {
	body := strings.Join([]string{
		"# HELP vllm:request_success_total Count of successful requests.",
		"# TYPE vllm:request_success_total counter",
		`vllm:request_success_total{model_name="llama"} 42.0`,
		`vllm:num_requests_running{model_name="llama"} 3.0`,
		`vllm:num_requests_waiting{model_name="llama"} 1.0`,
		"vllm:gpu_cache_usage_perc 0.5", // unrelated series, ignored
		"",
	}, "\n")

	m := parsePrometheusMetrics(strings.NewReader(body), metricsDialects["vllm"])
	if !m.hasTotal {
		t.Fatalf("expected hasTotal true")
	}
	if m.total != 42.0 {
		t.Fatalf("expected total 42, got %v", m.total)
	}
	// active = running(3) + waiting(1)
	if m.active != 4.0 {
		t.Fatalf("expected active 4, got %v", m.active)
	}
}

func TestParsePrometheusMetrics_SumsLabelledCounters(t *testing.T) {
	body := strings.Join([]string{
		`vllm:request_success_total{model_name="a"} 10`,
		`vllm:request_success_total{model_name="b"} 5`,
		"",
	}, "\n")
	m := parsePrometheusMetrics(strings.NewReader(body), metricsDialects["vllm"])
	if m.total != 15.0 {
		t.Fatalf("expected summed total 15, got %v", m.total)
	}
}

func TestParsePrometheusMetrics_NoTotal(t *testing.T) {
	body := "some_other_metric 1\n# a comment\n"
	m := parsePrometheusMetrics(strings.NewReader(body), metricsDialects["vllm"])
	if m.hasTotal {
		t.Fatalf("expected hasTotal false when no known counter present")
	}
}

func TestParseMetricLine(t *testing.T) {
	cases := []struct {
		line     string
		wantName string
		wantVal  float64
		wantOK   bool
	}{
		{`vllm:request_success_total{m="x"} 42.5`, "vllm:request_success_total", 42.5, true},
		{`plain_metric 7`, "plain_metric", 7, true},
		{`with_ts{a="b"} 3 1699999999000`, "with_ts", 3, true},
		{`# comment style`, "", 0, false},
		{`malformed`, "", 0, false},
		{`bad_value{a="b"} notanumber`, "", 0, false},
	}
	for _, c := range cases {
		name, val, ok := parseMetricLine(c.line)
		if ok != c.wantOK {
			t.Fatalf("line %q: ok=%v want %v", c.line, ok, c.wantOK)
		}
		if !ok {
			continue
		}
		if name != c.wantName || val != c.wantVal {
			t.Fatalf("line %q: got (%q,%v) want (%q,%v)", c.line, name, val, c.wantName, c.wantVal)
		}
	}
}

func TestIdleTracker_Observe_ScrapesLiveEndpoint(t *testing.T) {
	// Serve a vLLM-style /metrics response and confirm Observe wires the scrape
	// into the idle computation end to end.
	var total float64 = 5
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("vllm:request_success_total " + strconv.FormatFloat(total, 'f', -1, 64) + "\n" +
			"vllm:num_requests_running 0\nvllm:num_requests_waiting 0\n"))
	}))
	defer server.Close()

	port := portFromURL(t, server.URL)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tr := newTestTracker(300, &now)

	st, ok := tr.Observe(context.Background(), "vllm", "vllm", port)
	if !ok {
		t.Fatalf("expected Observe to succeed against live metrics endpoint")
	}
	if st.LastRequestAt != nil {
		t.Fatalf("expected no last_request_at on first scrape")
	}

	// A request completes; counter advances; time passes under threshold.
	total = 6
	now = now.Add(120 * time.Second)
	st, ok = tr.Observe(context.Background(), "vllm", "vllm", port)
	if !ok {
		t.Fatalf("expected second Observe to succeed")
	}
	if st.LastRequestAt == nil || !st.LastRequestAt.Equal(now) {
		t.Fatalf("expected last_request_at %v, got %v", now, st.LastRequestAt)
	}
	if st.Idle {
		t.Fatalf("expected not idle 0s after a request")
	}
}

func TestIdleTracker_Observe_UnknownEngine(t *testing.T) {
	now := time.Now()
	tr := newTestTracker(300, &now)
	if _, ok := tr.Observe(context.Background(), "ollama", "ollama", 11434); ok {
		t.Fatalf("expected Observe to fail for an engine with no metrics dialect")
	}
}

func TestIdleState_JSONPromotion(t *testing.T) {
	last := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	info := AppInfo{
		Name:   "vllm",
		Status: "running",
		Port:   8100,
		IdleState: &IdleState{
			Idle:          true,
			IdleSeconds:   600,
			LastRequestAt: &last,
		},
	}
	b, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Fields must be promoted to the top level, not nested under a struct.
	if got["idle"] != true {
		t.Fatalf("expected promoted idle=true, got %v (json=%s)", got["idle"], b)
	}
	if got["idle_seconds"].(float64) != 600 {
		t.Fatalf("expected idle_seconds 600, got %v", got["idle_seconds"])
	}
	if _, ok := got["last_request_at"]; !ok {
		t.Fatalf("expected last_request_at present, json=%s", b)
	}
}

func TestIdleState_OmittedWhenNil(t *testing.T) {
	info := AppInfo{Name: "postgres", Status: "running", Port: 5432}
	b, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "idle") {
		t.Fatalf("expected no idle fields when IdleState is nil, got %s", b)
	}
}

func TestIdleEngineType(t *testing.T) {
	if idleEngineType("my-vllm-app") != "vllm" {
		t.Fatalf("expected vllm engine for name containing vllm")
	}
	if idleEngineType("postgres") != "" {
		t.Fatalf("expected empty engine for non-inference name")
	}
}

func portFromURL(t *testing.T, url string) int {
	t.Helper()
	i := strings.LastIndex(url, ":")
	if i < 0 {
		t.Fatalf("no port in url %q", url)
	}
	p, err := strconv.Atoi(url[i+1:])
	if err != nil {
		t.Fatalf("bad port in url %q: %v", url, err)
	}
	return p
}
