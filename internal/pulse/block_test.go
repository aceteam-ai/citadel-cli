package pulse

import (
	"encoding/json"
	"testing"
)

// TestStatsBlockContractGolden pins the exact wire shape the aceteam backend
// is built against (aceteam-ai/aceteam#6334). Field names and omission
// behavior are load-bearing: if this test fails, the backend contract broke.
func TestStatsBlockContractGolden(t *testing.T) {
	block := StatsBlock{
		V:  StatsVersion,
		TS: 1753142400,
		GPUs: []GPUStat{{
			Index:      0,
			UtilPct:    float64Ptr(85),
			MemUsedMB:  intPtr(22528),
			MemTotalMB: intPtr(24576),
			TempC:      intPtr(68),
			PowerW:     float64Ptr(259),
		}},
		Inference: []InferenceStat{{
			Engine:     "vllm",
			Model:      "Qwen3-9B",
			Port:       8080,
			GenTPS:     float64Ptr(252.0),
			PromptTPS:  float64Ptr(0.0),
			KVCachePct: float64Ptr(77.7),
			Running:    intPtr(4),
			Waiting:    intPtr(0),
			TTFTMsP50:  float64Ptr(2500),
			E2EMsP50:   float64Ptr(20000),
		}},
	}

	got, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"v":1,"ts":1753142400,` +
		`"gpus":[{"i":0,"util_pct":85,"mem_used_mb":22528,"mem_total_mb":24576,"temp_c":68,"power_w":259}],` +
		`"inference":[{"engine":"vllm","model":"Qwen3-9B","port":8080,"gen_tps":252,"prompt_tps":0,"kv_cache_pct":77.7,"running":4,"waiting":0,"ttft_ms_p50":2500,"e2e_ms_p50":20000}]}`
	if string(got) != want {
		t.Errorf("contract shape drifted:\n got: %s\nwant: %s", got, want)
	}
}

// TestStatsBlockOmission verifies that empty arrays and unavailable fields are
// omitted entirely, never zero-filled.
func TestStatsBlockOmission(t *testing.T) {
	t.Run("no gpus, no inference", func(t *testing.T) {
		got, err := json.Marshal(StatsBlock{V: 1, TS: 1753142400})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if want := `{"v":1,"ts":1753142400}`; string(got) != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})

	t.Run("gpu entry with only index", func(t *testing.T) {
		got, err := json.Marshal(GPUStat{Index: 1})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if want := `{"i":1}`; string(got) != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})

	t.Run("present zero is shipped, absent is omitted", func(t *testing.T) {
		got, err := json.Marshal(InferenceStat{
			Engine:  "sglang",
			Port:    30000,
			Waiting: intPtr(0),
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if want := `{"engine":"sglang","port":30000,"waiting":0}`; string(got) != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})
}
