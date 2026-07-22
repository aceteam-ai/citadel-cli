package pulse

import (
	"math"
	"strings"
	"testing"
)

func TestParsePromText(t *testing.T) {
	text := `
# HELP vllm:num_requests_running Number of requests currently running.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{model_name="Qwen/Qwen3-9B"} 4.0
plain_gauge 1.5
vllm:time_to_first_token_seconds_bucket{le="0.5",model_name="Qwen/Qwen3-9B"} 10
vllm:time_to_first_token_seconds_bucket{le="+Inf",model_name="Qwen/Qwen3-9B"} 12
escaped{name="he said \"hi\", twice"} 2
with_timestamp{a="b"} 3.14 1753142400000
malformed_no_value{
garbage line
`
	samples := parsePromText(strings.NewReader(text))
	if len(samples) != 6 {
		t.Fatalf("expected 6 samples, got %d: %+v", len(samples), samples)
	}

	byName := make(map[string][]promSample)
	for _, s := range samples {
		byName[s.name] = append(byName[s.name], s)
	}

	running := byName["vllm:num_requests_running"]
	if len(running) != 1 || running[0].value != 4.0 {
		t.Errorf("running: got %+v", running)
	}
	if running[0].labels["model_name"] != "Qwen/Qwen3-9B" {
		t.Errorf("model_name label: got %q", running[0].labels["model_name"])
	}

	if v := byName["plain_gauge"]; len(v) != 1 || v[0].value != 1.5 || v[0].labels != nil {
		t.Errorf("plain_gauge: got %+v", v)
	}

	buckets := byName["vllm:time_to_first_token_seconds_bucket"]
	if len(buckets) != 2 {
		t.Fatalf("buckets: got %+v", buckets)
	}
	if buckets[0].labels["le"] != "0.5" || buckets[1].labels["le"] != "+Inf" {
		t.Errorf("le labels: got %q, %q", buckets[0].labels["le"], buckets[1].labels["le"])
	}

	if v := byName["escaped"]; len(v) != 1 || v[0].labels["name"] != `he said "hi", twice` {
		t.Errorf("escaped label: got %+v", v)
	}

	if v := byName["with_timestamp"]; len(v) != 1 || v[0].value != 3.14 {
		t.Errorf("with_timestamp: got %+v", v)
	}
}

func TestParseLe(t *testing.T) {
	if v, err := parseLe("+Inf"); err != nil || !math.IsInf(v, 1) {
		t.Errorf("+Inf: got %v, %v", v, err)
	}
	if v, err := parseLe("0.5"); err != nil || v != 0.5 {
		t.Errorf("0.5: got %v, %v", v, err)
	}
	if _, err := parseLe("bogus"); err == nil {
		t.Error("bogus le should error")
	}
}
