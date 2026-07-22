package pulse

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// vllmMetricsT0 / vllmMetricsT1 are canned vLLM /metrics expositions 10s
// apart. The modern dialect: no throughput gauges, so tokens/s must come from
// counter deltas, and latency medians from histogram bucket deltas.
//
// Windowed math the test asserts (t0 -> t1):
//   - generation_tokens_total 50000 -> 52520 over 10s  => gen_tps 252.0
//   - prompt_tokens_total     10000 -> 10000 over 10s  => prompt_tps 0.0
//   - TTFT bucket deltas: le=1:+0, le=2:+1, le=3:+3, +Inf:+4 (count +4)
//     median target 2 falls in le=3: 2 + (3-2)*(2-1)/2 = 2.5s => 2500ms
//   - e2e bucket deltas: le=10:+0, le=20:+2, le=30:+4, +Inf:+4
//     median target 2 falls in le=20: 10 + (20-10)*(2-0)/2 = 20s => 20000ms
const vllmMetricsT0 = `# HELP vllm:num_requests_running Number of requests currently running on GPU.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{model_name="Qwen/Qwen3-9B"} 4.0
vllm:num_requests_waiting{model_name="Qwen/Qwen3-9B"} 0.0
vllm:gpu_cache_usage_perc{model_name="Qwen/Qwen3-9B"} 0.777
vllm:prompt_tokens_total{model_name="Qwen/Qwen3-9B"} 10000.0
vllm:generation_tokens_total{model_name="Qwen/Qwen3-9B"} 50000.0
vllm:time_to_first_token_seconds_bucket{le="1",model_name="Qwen/Qwen3-9B"} 100
vllm:time_to_first_token_seconds_bucket{le="2",model_name="Qwen/Qwen3-9B"} 150
vllm:time_to_first_token_seconds_bucket{le="3",model_name="Qwen/Qwen3-9B"} 190
vllm:time_to_first_token_seconds_bucket{le="+Inf",model_name="Qwen/Qwen3-9B"} 200
vllm:time_to_first_token_seconds_count{model_name="Qwen/Qwen3-9B"} 200
vllm:e2e_request_latency_seconds_bucket{le="10",model_name="Qwen/Qwen3-9B"} 40
vllm:e2e_request_latency_seconds_bucket{le="20",model_name="Qwen/Qwen3-9B"} 120
vllm:e2e_request_latency_seconds_bucket{le="30",model_name="Qwen/Qwen3-9B"} 195
vllm:e2e_request_latency_seconds_bucket{le="+Inf",model_name="Qwen/Qwen3-9B"} 200
vllm:e2e_request_latency_seconds_count{model_name="Qwen/Qwen3-9B"} 200
`

const vllmMetricsT1 = `vllm:num_requests_running{model_name="Qwen/Qwen3-9B"} 4.0
vllm:num_requests_waiting{model_name="Qwen/Qwen3-9B"} 0.0
vllm:gpu_cache_usage_perc{model_name="Qwen/Qwen3-9B"} 0.777
vllm:prompt_tokens_total{model_name="Qwen/Qwen3-9B"} 10000.0
vllm:generation_tokens_total{model_name="Qwen/Qwen3-9B"} 52520.0
vllm:time_to_first_token_seconds_bucket{le="1",model_name="Qwen/Qwen3-9B"} 100
vllm:time_to_first_token_seconds_bucket{le="2",model_name="Qwen/Qwen3-9B"} 151
vllm:time_to_first_token_seconds_bucket{le="3",model_name="Qwen/Qwen3-9B"} 193
vllm:time_to_first_token_seconds_bucket{le="+Inf",model_name="Qwen/Qwen3-9B"} 204
vllm:time_to_first_token_seconds_count{model_name="Qwen/Qwen3-9B"} 204
vllm:e2e_request_latency_seconds_bucket{le="10",model_name="Qwen/Qwen3-9B"} 40
vllm:e2e_request_latency_seconds_bucket{le="20",model_name="Qwen/Qwen3-9B"} 122
vllm:e2e_request_latency_seconds_bucket{le="30",model_name="Qwen/Qwen3-9B"} 199
vllm:e2e_request_latency_seconds_bucket{le="+Inf",model_name="Qwen/Qwen3-9B"} 204
vllm:e2e_request_latency_seconds_count{model_name="Qwen/Qwen3-9B"} 204
`

// vllmLegacyMetrics is the older vLLM dialect that still exposed throughput
// gauges directly.
const vllmLegacyMetrics = `vllm:avg_generation_throughput_toks_per_s{model_name="mistral-7b"} 252.0
vllm:avg_prompt_throughput_toks_per_s{model_name="mistral-7b"} 0.0
vllm:gpu_cache_usage_perc{model_name="mistral-7b"} 0.5
vllm:num_requests_running{model_name="mistral-7b"} 2.0
vllm:num_requests_waiting{model_name="mistral-7b"} 1.0
`

// sglangMetrics is a canned SGLang exposition: gen throughput gauge,
// token_usage fraction, running/queued gauges.
const sglangMetrics = `# TYPE sglang:gen_throughput gauge
sglang:gen_throughput{model_name="meta-llama/Llama-3.1-8B-Instruct"} 117.3
sglang:token_usage{model_name="meta-llama/Llama-3.1-8B-Instruct"} 0.284
sglang:num_running_reqs{model_name="meta-llama/Llama-3.1-8B-Instruct"} 3.0
sglang:num_queue_reqs{model_name="meta-llama/Llama-3.1-8B-Instruct"} 0.0
sglang:prompt_tokens_total{model_name="meta-llama/Llama-3.1-8B-Instruct"} 5000.0
sglang:generation_tokens_total{model_name="meta-llama/Llama-3.1-8B-Instruct"} 20000.0
`

// metricsServer serves a swappable metrics body and returns a scraper aimed
// at it.
func metricsServer(t *testing.T, engine string) (*httptest.Server, *engineScraper, *string) {
	t.Helper()
	body := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body == "" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("test server port: %v", err)
	}
	return srv, newEngineScraper(engine, port, time.Second), &body
}

func TestVLLMCounterRateAndHistogramP50(t *testing.T) {
	_, scraper, body := metricsServer(t, "vllm")

	t0 := time.Unix(1753142400, 0)
	scraper.now = func() time.Time { return t0 }

	*body = vllmMetricsT0
	stat := scraper.Observe(context.Background())
	if stat == nil {
		t.Fatal("first scrape returned nil")
	}
	// First scrape has no baseline: gauges present, rates/percentiles omitted.
	if stat.Engine != "vllm" {
		t.Errorf("engine: got %q", stat.Engine)
	}
	if stat.Model != "Qwen/Qwen3-9B" {
		t.Errorf("model: got %q", stat.Model)
	}
	if stat.Running == nil || *stat.Running != 4 {
		t.Errorf("running: got %v", stat.Running)
	}
	if stat.Waiting == nil || *stat.Waiting != 0 {
		t.Errorf("waiting: got %v", stat.Waiting)
	}
	if stat.KVCachePct == nil || *stat.KVCachePct != 77.7 {
		t.Errorf("kv_cache_pct: got %v", stat.KVCachePct)
	}
	if stat.GenTPS != nil || stat.PromptTPS != nil {
		t.Errorf("first scrape must omit counter-derived tps, got gen=%v prompt=%v", stat.GenTPS, stat.PromptTPS)
	}
	if stat.TTFTMsP50 != nil || stat.E2EMsP50 != nil {
		t.Errorf("first scrape must omit windowed p50s, got ttft=%v e2e=%v", stat.TTFTMsP50, stat.E2EMsP50)
	}

	// Second scrape, 10s later: rates and windowed medians appear.
	scraper.now = func() time.Time { return t0.Add(10 * time.Second) }
	*body = vllmMetricsT1
	stat = scraper.Observe(context.Background())
	if stat == nil {
		t.Fatal("second scrape returned nil")
	}
	if stat.GenTPS == nil || *stat.GenTPS != 252.0 {
		t.Errorf("gen_tps: got %v, want 252.0", deref(stat.GenTPS))
	}
	if stat.PromptTPS == nil || *stat.PromptTPS != 0.0 {
		t.Errorf("prompt_tps: got %v, want 0.0", deref(stat.PromptTPS))
	}
	if stat.TTFTMsP50 == nil || *stat.TTFTMsP50 != 2500 {
		t.Errorf("ttft_ms_p50: got %v, want 2500", deref(stat.TTFTMsP50))
	}
	if stat.E2EMsP50 == nil || *stat.E2EMsP50 != 20000 {
		t.Errorf("e2e_ms_p50: got %v, want 20000", deref(stat.E2EMsP50))
	}
}

func TestVLLMLegacyThroughputGauges(t *testing.T) {
	_, scraper, body := metricsServer(t, "vllm")
	*body = vllmLegacyMetrics

	stat := scraper.Observe(context.Background())
	if stat == nil {
		t.Fatal("scrape returned nil")
	}
	// Gauges are direct: available on the very first scrape, no baseline needed.
	if stat.GenTPS == nil || *stat.GenTPS != 252.0 {
		t.Errorf("gen_tps: got %v, want 252.0", deref(stat.GenTPS))
	}
	if stat.PromptTPS == nil || *stat.PromptTPS != 0.0 {
		t.Errorf("prompt_tps: got %v, want 0.0", deref(stat.PromptTPS))
	}
	if stat.KVCachePct == nil || *stat.KVCachePct != 50.0 {
		t.Errorf("kv_cache_pct: got %v, want 50.0", deref(stat.KVCachePct))
	}
	if stat.Running == nil || *stat.Running != 2 || stat.Waiting == nil || *stat.Waiting != 1 {
		t.Errorf("queues: running=%v waiting=%v", stat.Running, stat.Waiting)
	}
	if stat.Model != "mistral-7b" {
		t.Errorf("model: got %q", stat.Model)
	}
	// Latency histograms absent from this exposition: fields omitted.
	if stat.TTFTMsP50 != nil || stat.E2EMsP50 != nil {
		t.Errorf("expected omitted p50s, got ttft=%v e2e=%v", stat.TTFTMsP50, stat.E2EMsP50)
	}
}

func TestSGLangDialect(t *testing.T) {
	_, scraper, body := metricsServer(t, "sglang")
	*body = sglangMetrics

	stat := scraper.Observe(context.Background())
	if stat == nil {
		t.Fatal("scrape returned nil")
	}
	if stat.Engine != "sglang" {
		t.Errorf("engine: got %q", stat.Engine)
	}
	if stat.Model != "meta-llama/Llama-3.1-8B-Instruct" {
		t.Errorf("model: got %q", stat.Model)
	}
	if stat.GenTPS == nil || *stat.GenTPS != 117.3 {
		t.Errorf("gen_tps: got %v, want 117.3", deref(stat.GenTPS))
	}
	if stat.KVCachePct == nil || *stat.KVCachePct != 28.4 {
		t.Errorf("kv_cache_pct: got %v, want 28.4", deref(stat.KVCachePct))
	}
	if stat.Running == nil || *stat.Running != 3 || stat.Waiting == nil || *stat.Waiting != 0 {
		t.Errorf("queues: running=%v waiting=%v", stat.Running, stat.Waiting)
	}
	// SGLang has no prompt throughput gauge and this is the first scrape:
	// prompt_tps omitted, not zero-filled.
	if stat.PromptTPS != nil {
		t.Errorf("prompt_tps must be omitted on first scrape, got %v", *stat.PromptTPS)
	}
}

func TestScrapeFailuresReturnNil(t *testing.T) {
	t.Run("endpoint absent", func(t *testing.T) {
		// A port with nothing listening: connection refused, silently nil.
		scraper := newEngineScraper("vllm", 1, 200*time.Millisecond)
		if stat := scraper.Observe(context.Background()); stat != nil {
			t.Errorf("expected nil for absent endpoint, got %+v", stat)
		}
	})

	t.Run("non-200 response", func(t *testing.T) {
		_, scraper, body := metricsServer(t, "vllm")
		*body = "" // server 404s on empty body
		if stat := scraper.Observe(context.Background()); stat != nil {
			t.Errorf("expected nil for 404, got %+v", stat)
		}
	})

	t.Run("wrong service on port", func(t *testing.T) {
		_, scraper, body := metricsServer(t, "vllm")
		*body = "some_other_service_metric 1.0\n"
		if stat := scraper.Observe(context.Background()); stat != nil {
			t.Errorf("expected nil when no dialect series match, got %+v", stat)
		}
	})

	t.Run("failure drops the rate baseline", func(t *testing.T) {
		_, scraper, body := metricsServer(t, "vllm")
		*body = vllmMetricsT0
		if stat := scraper.Observe(context.Background()); stat == nil {
			t.Fatal("seed scrape failed")
		}
		*body = "" // engine goes away (404)
		if stat := scraper.Observe(context.Background()); stat != nil {
			t.Fatalf("expected nil during outage, got %+v", stat)
		}
		// Engine comes back: no rates on the first post-outage scrape.
		*body = vllmMetricsT1
		stat := scraper.Observe(context.Background())
		if stat == nil {
			t.Fatal("post-outage scrape failed")
		}
		if stat.GenTPS != nil || stat.TTFTMsP50 != nil {
			t.Errorf("baseline must reset across an outage, got gen_tps=%v ttft=%v", deref(stat.GenTPS), deref(stat.TTFTMsP50))
		}
	})
}

func TestCounterRate(t *testing.T) {
	if v, ok := counterRate(100, 350, 10); !ok || v != 25.0 {
		t.Errorf("got %v, %v", v, ok)
	}
	if _, ok := counterRate(350, 100, 10); ok {
		t.Error("counter reset must yield no rate")
	}
	if _, ok := counterRate(100, 200, 0); ok {
		t.Error("zero window must yield no rate")
	}
}

func TestHistogramP50Delta(t *testing.T) {
	inf := math.Inf(1)
	snap := func(count float64, b map[float64]float64) histSnapshot {
		return histSnapshot{buckets: b, count: count, has: true}
	}

	t.Run("no new samples", func(t *testing.T) {
		s := snap(10, map[float64]float64{1: 5, inf: 10})
		if _, ok := histogramP50Delta(s, s); ok {
			t.Error("identical snapshots must yield no p50")
		}
	})

	t.Run("median in overflow bucket reports highest finite bound", func(t *testing.T) {
		prev := snap(0, map[float64]float64{1: 0, 2: 0, inf: 0})
		cur := snap(10, map[float64]float64{1: 1, 2: 2, inf: 10})
		p50, ok := histogramP50Delta(prev, cur)
		if !ok || p50 != 2 {
			t.Errorf("got %v, %v; want 2, true", p50, ok)
		}
	})

	t.Run("counter reset yields no p50", func(t *testing.T) {
		prev := snap(100, map[float64]float64{1: 50, inf: 100})
		cur := snap(110, map[float64]float64{1: 3, inf: 110})
		if _, ok := histogramP50Delta(prev, cur); ok {
			t.Error("bucket going backwards must yield no p50")
		}
	})

	t.Run("missing snapshot", func(t *testing.T) {
		cur := snap(10, map[float64]float64{1: 10, inf: 10})
		if _, ok := histogramP50Delta(histSnapshot{}, cur); ok {
			t.Error("missing prev must yield no p50")
		}
	})
}

// deref formats a *float64 for error messages without nil panics.
func deref(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}
