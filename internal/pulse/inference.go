package pulse

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"
)

// engineDialect names the Prometheus series that carry each contract field for
// one inference engine. Any slice may be empty when the engine has no such
// signal; the corresponding contract field is then omitted.
//
// Metric-name drift across engine versions is handled by listing every known
// spelling; the first family that yields data wins.
type engineDialect struct {
	// genTPSGauges / promptTPSGauges are direct tokens-per-second gauges
	// (older vLLM exposed these; SGLang exposes gen_throughput).
	genTPSGauges    []string
	promptTPSGauges []string
	// genTokenCounters / promptTokenCounters are monotonic token counters used
	// to compute a rate across consecutive scrapes when no gauge is exposed.
	genTokenCounters    []string
	promptTokenCounters []string
	// kvCacheFractionGauges report KV-cache utilization as a 0-1 fraction.
	kvCacheFractionGauges []string
	// runningGauges / waitingGauges are live request-queue gauges.
	runningGauges []string
	waitingGauges []string
	// ttftHist / e2eHist are histogram base names (seconds) for
	// time-to-first-token and end-to-end request latency.
	ttftHist string
	e2eHist  string
}

// dialects maps engine name -> metrics dialect.
//
// vLLM (OpenAI server, /metrics):
//   - throughput: vllm:avg_generation_throughput_toks_per_s and
//     vllm:avg_prompt_throughput_toks_per_s were removed in newer versions in
//     favor of the counters vllm:generation_tokens_total /
//     vllm:prompt_tokens_total, so both paths are listed.
//   - KV cache: vllm:gpu_cache_usage_perc (V0) / vllm:kv_cache_usage_perc (V1),
//     both 0-1 fractions despite the "perc" name.
//   - queues: vllm:num_requests_running / vllm:num_requests_waiting.
//   - latency: vllm:time_to_first_token_seconds and
//     vllm:e2e_request_latency_seconds histograms.
//
// SGLang (/metrics): sglang:gen_throughput gauge (tok/s),
// sglang:prompt_tokens_total / sglang:generation_tokens_total counters,
// sglang:token_usage (0-1 KV token capacity in use),
// sglang:num_running_reqs / sglang:num_queue_reqs, and the same two latency
// histogram names under the sglang: prefix. SGLang exposes no prompt
// throughput gauge, so prompt_tps always comes from the counter rate.
var dialects = map[string]engineDialect{
	"vllm": {
		genTPSGauges:          []string{"vllm:avg_generation_throughput_toks_per_s"},
		promptTPSGauges:       []string{"vllm:avg_prompt_throughput_toks_per_s"},
		genTokenCounters:      []string{"vllm:generation_tokens_total"},
		promptTokenCounters:   []string{"vllm:prompt_tokens_total"},
		kvCacheFractionGauges: []string{"vllm:gpu_cache_usage_perc", "vllm:kv_cache_usage_perc"},
		runningGauges:         []string{"vllm:num_requests_running"},
		waitingGauges:         []string{"vllm:num_requests_waiting"},
		ttftHist:              "vllm:time_to_first_token_seconds",
		e2eHist:               "vllm:e2e_request_latency_seconds",
	},
	"sglang": {
		genTPSGauges:          []string{"sglang:gen_throughput"},
		genTokenCounters:      []string{"sglang:generation_tokens_total"},
		promptTokenCounters:   []string{"sglang:prompt_tokens_total"},
		kvCacheFractionGauges: []string{"sglang:token_usage"},
		runningGauges:         []string{"sglang:num_running_reqs"},
		waitingGauges:         []string{"sglang:num_queue_reqs"},
		ttftHist:              "sglang:time_to_first_token_seconds",
		e2eHist:               "sglang:e2e_request_latency_seconds",
	},
}

// modelNameLabel is the label both vLLM and SGLang attach to their series to
// identify the served model.
const modelNameLabel = "model_name"

// engineScraper scrapes one engine's /metrics endpoint and folds consecutive
// samples into an InferenceStat. It keeps the previous sample as the baseline
// for counter rates and windowed histogram percentiles. Not safe for
// concurrent use; the Collector calls it from a single goroutine.
type engineScraper struct {
	engine  string
	port    int
	dialect engineDialect
	client  *http.Client
	now     func() time.Time

	prev *scrapeSample
}

// scrapeSample is the rate/percentile baseline retained between scrapes.
type scrapeSample struct {
	at time.Time

	genTokens       float64
	hasGenTokens    bool
	promptTokens    float64
	hasPromptTokens bool

	ttft histSnapshot
	e2e  histSnapshot
}

// histSnapshot is a cumulative histogram state: bucket upper bound (le) ->
// cumulative count, summed across label sets.
type histSnapshot struct {
	buckets map[float64]float64
	count   float64
	has     bool
}

func newEngineScraper(engine string, port int, timeout time.Duration) *engineScraper {
	return &engineScraper{
		engine:  engine,
		port:    port,
		dialect: dialects[engine],
		client:  &http.Client{Timeout: timeout},
		now:     time.Now,
	}
}

// Observe scrapes the engine's metrics endpoint and returns the current
// InferenceStat, or nil when the engine is absent, unreachable, warming, or
// exposes none of the dialect's series. Failures also drop the rate baseline
// so a restarted engine never produces rates computed across the gap.
func (s *engineScraper) Observe(ctx context.Context) *InferenceStat {
	samples, err := s.fetch(ctx)
	if err != nil {
		s.prev = nil
		return nil
	}

	byName := make(map[string][]promSample)
	for _, sm := range samples {
		byName[sm.name] = append(byName[sm.name], sm)
	}

	stat := &InferenceStat{Engine: s.engine, Port: s.port}
	matched := false

	// Direct gauges.
	if v, ok := sumGauges(byName, s.dialect.genTPSGauges); ok {
		stat.GenTPS = float64Ptr(v)
		matched = true
	}
	if v, ok := sumGauges(byName, s.dialect.promptTPSGauges); ok {
		stat.PromptTPS = float64Ptr(v)
		matched = true
	}
	if v, ok := sumGauges(byName, s.dialect.kvCacheFractionGauges); ok {
		stat.KVCachePct = float64Ptr(round1(v * 100))
		matched = true
	}
	if v, ok := sumGauges(byName, s.dialect.runningGauges); ok {
		stat.Running = intPtr(int(math.Round(v)))
		matched = true
	}
	if v, ok := sumGauges(byName, s.dialect.waitingGauges); ok {
		stat.Waiting = intPtr(int(math.Round(v)))
		matched = true
	}

	// Served model, from the model_name label on any dialect series.
	stat.Model = findModelName(byName, s.dialect)

	// Rate/percentile fields need a previous sample as the baseline.
	cur := scrapeSample{at: s.now()}
	cur.genTokens, cur.hasGenTokens = sumGauges(byName, s.dialect.genTokenCounters)
	cur.promptTokens, cur.hasPromptTokens = sumGauges(byName, s.dialect.promptTokenCounters)
	cur.ttft = snapshotHistogram(byName, s.dialect.ttftHist)
	cur.e2e = snapshotHistogram(byName, s.dialect.e2eHist)
	matched = matched || cur.hasGenTokens || cur.hasPromptTokens || cur.ttft.has || cur.e2e.has

	if s.prev != nil {
		dt := cur.at.Sub(s.prev.at).Seconds()
		// Counter-derived throughput fills in only when no gauge was exposed.
		if stat.GenTPS == nil && cur.hasGenTokens && s.prev.hasGenTokens {
			if rate, ok := counterRate(s.prev.genTokens, cur.genTokens, dt); ok {
				stat.GenTPS = float64Ptr(rate)
			}
		}
		if stat.PromptTPS == nil && cur.hasPromptTokens && s.prev.hasPromptTokens {
			if rate, ok := counterRate(s.prev.promptTokens, cur.promptTokens, dt); ok {
				stat.PromptTPS = float64Ptr(rate)
			}
		}
		if p50, ok := histogramP50Delta(s.prev.ttft, cur.ttft); ok {
			stat.TTFTMsP50 = float64Ptr(round1(p50 * 1000))
		}
		if p50, ok := histogramP50Delta(s.prev.e2e, cur.e2e); ok {
			stat.E2EMsP50 = float64Ptr(round1(p50 * 1000))
		}
	}
	s.prev = &cur

	// Nothing recognized at all: some other service answered on this port.
	// Report nothing rather than a hollow entry.
	if !matched {
		return nil
	}
	return stat
}

// fetch GETs the engine's /metrics endpoint on loopback and parses it.
func (s *engineScraper) fetch(ctx context.Context) ([]promSample, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/metrics", s.port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics endpoint returned %d", resp.StatusCode)
	}
	return parsePromText(resp.Body), nil
}

// sumGauges sums every series of the first metric family (in listed priority
// order) that is present. Summing across label sets is correct both for
// per-label counter fan-out and for single-series gauges.
func sumGauges(byName map[string][]promSample, names []string) (float64, bool) {
	for _, name := range names {
		samples, ok := byName[name]
		if !ok || len(samples) == 0 {
			continue
		}
		var sum float64
		for _, s := range samples {
			sum += s.value
		}
		return sum, true
	}
	return 0, false
}

// findModelName returns the model_name label from the first dialect series
// that carries one, or "".
func findModelName(byName map[string][]promSample, d engineDialect) string {
	families := make([]string, 0, 16)
	families = append(families, d.runningGauges...)
	families = append(families, d.waitingGauges...)
	families = append(families, d.kvCacheFractionGauges...)
	families = append(families, d.genTokenCounters...)
	families = append(families, d.promptTokenCounters...)
	families = append(families, d.genTPSGauges...)
	families = append(families, d.promptTPSGauges...)
	for _, name := range families {
		for _, s := range byName[name] {
			if m := s.labels[modelNameLabel]; m != "" {
				return m
			}
		}
	}
	return ""
}

// snapshotHistogram folds a histogram family's _bucket/_count series into a
// cumulative snapshot, summing across label sets per bucket bound.
func snapshotHistogram(byName map[string][]promSample, base string) histSnapshot {
	if base == "" {
		return histSnapshot{}
	}
	snap := histSnapshot{buckets: make(map[float64]float64)}
	for _, s := range byName[base+"_bucket"] {
		le, err := parseLe(s.labels["le"])
		if err != nil {
			continue
		}
		snap.buckets[le] += s.value
		snap.has = true
	}
	for _, s := range byName[base+"_count"] {
		snap.count += s.value
	}
	// Some expositions omit _count; fall back to the +Inf bucket.
	if snap.has && snap.count == 0 {
		snap.count = snap.buckets[math.Inf(1)]
	}
	return snap
}

func parseLe(v string) (float64, error) {
	if v == "+Inf" {
		return math.Inf(1), nil
	}
	var f float64
	_, err := fmt.Sscanf(v, "%g", &f)
	return f, err
}

// counterRate computes a per-second rate from two monotonic counter readings.
// A negative delta (engine restart / counter reset) or non-positive window
// yields no rate.
func counterRate(prev, cur, dtSeconds float64) (float64, bool) {
	if dtSeconds <= 0 || cur < prev {
		return 0, false
	}
	return round1((cur - prev) / dtSeconds), true
}

// histogramP50Delta computes the median over the window between two cumulative
// histogram snapshots, using standard Prometheus-style linear interpolation
// within the median bucket. Returns ok=false when either snapshot is missing,
// no request finished in the window, or the counters went backwards (reset).
func histogramP50Delta(prev, cur histSnapshot) (float64, bool) {
	if !prev.has || !cur.has {
		return 0, false
	}
	totalDelta := cur.count - prev.count
	if totalDelta <= 0 {
		return 0, false
	}

	les := make([]float64, 0, len(cur.buckets))
	for le := range cur.buckets {
		les = append(les, le)
	}
	sort.Float64s(les)

	target := totalDelta / 2
	prevBound := 0.0
	prevCum := 0.0
	for _, le := range les {
		cum := cur.buckets[le] - prev.buckets[le]
		if cum < prevCum {
			// Bucket counts went backwards: counter reset mid-window.
			return 0, false
		}
		if cum >= target {
			if math.IsInf(le, 1) {
				// Median falls in the overflow bucket: report the highest
				// finite bound as the best available estimate (matches
				// histogram_quantile's behavior).
				return prevBound, true
			}
			bucketCount := cum - prevCum
			if bucketCount <= 0 {
				return le, true
			}
			return prevBound + (le-prevBound)*(target-prevCum)/bucketCount, true
		}
		prevBound = le
		prevCum = cum
	}
	return 0, false
}

// round1 rounds to one decimal place to keep the on-wire block compact.
func round1(v float64) float64 {
	return math.Round(v*10) / 10
}
