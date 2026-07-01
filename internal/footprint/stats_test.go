package footprint

import (
	"math"
	"testing"
)

func TestParseStatsJSONDockerLines(t *testing.T) {
	// Docker emits one JSON object per line.
	out := []byte(`{"Name":"vllm-vllm-1","CPUPerc":"12.34%","MemUsage":"7.4GiB / 62.5GiB"}
{"Name":"redis-redis-1","CPUPerc":"0.50%","MemUsage":"12MiB / 62.5GiB"}`)
	stats := parseStatsJSON(out)
	if len(stats) != 2 {
		t.Fatalf("expected 2 stats, got %d", len(stats))
	}
	if stats[0].Name != "vllm-vllm-1" || stats[0].CPUPerc != "12.34%" {
		t.Fatalf("unexpected first stat: %+v", stats[0])
	}
}

func TestParseStatsJSONPodmanArray(t *testing.T) {
	// Podman may emit a single JSON array.
	out := []byte(`[{"Name":"vllm","CPUPerc":"5.0%","MemUsage":"1.0GiB / 8GiB"}]`)
	stats := parseStatsJSON(out)
	if len(stats) != 1 || stats[0].Name != "vllm" {
		t.Fatalf("podman array parse failed: %+v", stats)
	}
}

func TestParseStatsJSONEmpty(t *testing.T) {
	if s := parseStatsJSON([]byte("  \n ")); s != nil {
		t.Fatalf("expected nil for empty output, got %v", s)
	}
}

func TestParseCPUPercent(t *testing.T) {
	cases := map[string]struct {
		want float64
		ok   bool
	}{
		"12.34%": {12.34, true},
		"0.00%":  {0, true},
		"":       {0, false},
		"n/a":    {0, false},
	}
	for in, exp := range cases {
		got, ok := parseCPUPercent(in)
		if ok != exp.ok || (ok && math.Abs(got-exp.want) > 1e-9) {
			t.Errorf("parseCPUPercent(%q) = (%v, %v), want (%v, %v)", in, got, ok, exp.want, exp.ok)
		}
	}
}

func TestParseMemUsageMB(t *testing.T) {
	cases := map[string]struct {
		want float64 // MB
		ok   bool
	}{
		"7.4GiB / 62.5GiB": {7.4 * 1024, true},                           // 7577.6 MB
		"512MiB / 16GiB":   {512, true},                                  // exact
		"1GB / 8GB":        {1000 * 1000 * 1000.0 / (1024 * 1024), true}, // SI GB
		"0B / 8GiB":        {0, true},
		"":                 {0, false},
		"garbage":          {0, false},
	}
	for in, exp := range cases {
		got, ok := parseMemUsageMB(in)
		if ok != exp.ok {
			t.Errorf("parseMemUsageMB(%q) ok = %v, want %v", in, ok, exp.ok)
			continue
		}
		if ok && math.Abs(got-exp.want) > 0.5 {
			t.Errorf("parseMemUsageMB(%q) = %.3f MB, want %.3f MB", in, got, exp.want)
		}
	}
}

func TestParseMBField(t *testing.T) {
	cases := map[string]struct {
		want int
		ok   bool
	}{
		"8192 MB": {8192, true},
		"8192MB":  {8192, true},
		"0 MB":    {0, true},
		"":        {0, false},
		"n/a":     {0, false},
	}
	for in, exp := range cases {
		got, ok := parseMBField(in)
		if ok != exp.ok || (ok && got != exp.want) {
			t.Errorf("parseMBField(%q) = (%d, %v), want (%d, %v)", in, got, ok, exp.want, exp.ok)
		}
	}
}

func TestParsePercentField(t *testing.T) {
	cases := map[string]struct {
		want float64
		ok   bool
	}{
		"85%": {85, true},
		"0%":  {0, true},
		"":    {0, false},
		"n/a": {0, false},
	}
	for in, exp := range cases {
		got, ok := parsePercentField(in)
		if ok != exp.ok || (ok && got != exp.want) {
			t.Errorf("parsePercentField(%q) = (%v, %v), want (%v, %v)", in, got, ok, exp.want, exp.ok)
		}
	}
}

func TestMatchContainer(t *testing.T) {
	stats := []containerStat{
		{Name: "diffusers-diffusers-1"},
		{Name: "vllm-server-1"},
	}
	if cs, ok := matchContainer(stats, "vllm"); !ok || cs.Name != "vllm-server-1" {
		t.Fatalf("expected to match vllm, got %+v ok=%v", cs, ok)
	}
	if _, ok := matchContainer(stats, "ollama"); ok {
		t.Fatalf("did not expect to match ollama")
	}
}
