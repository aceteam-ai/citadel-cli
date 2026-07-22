package pulse

import (
	"encoding/json"
	"testing"
)

func TestParseNvidiaSMICSV(t *testing.T) {
	t.Run("two gpus", func(t *testing.T) {
		out := "0, 85, 22528, 24576, 68, 259.42\n1, 0, 4, 24576, 31, 18.66\n"
		gpus := parseNvidiaSMICSV(out)
		if len(gpus) != 2 {
			t.Fatalf("expected 2 gpus, got %d", len(gpus))
		}
		g := gpus[0]
		if g.Index != 0 ||
			g.UtilPct == nil || *g.UtilPct != 85 ||
			g.MemUsedMB == nil || *g.MemUsedMB != 22528 ||
			g.MemTotalMB == nil || *g.MemTotalMB != 24576 ||
			g.TempC == nil || *g.TempC != 68 ||
			g.PowerW == nil || *g.PowerW != 259.4 {
			b, _ := json.Marshal(g)
			t.Errorf("gpu 0 parsed wrong: %s", b)
		}
		if gpus[1].Index != 1 {
			t.Errorf("gpu 1 index: got %d", gpus[1].Index)
		}
	})

	t.Run("unavailable fields are omitted, not zero-filled", func(t *testing.T) {
		out := "0, 85, 22528, 24576, [N/A], [Not Supported]\n"
		gpus := parseNvidiaSMICSV(out)
		if len(gpus) != 1 {
			t.Fatalf("expected 1 gpu, got %d", len(gpus))
		}
		if gpus[0].TempC != nil || gpus[0].PowerW != nil {
			t.Errorf("N/A fields must be nil: temp=%v power=%v", gpus[0].TempC, gpus[0].PowerW)
		}
		b, err := json.Marshal(gpus[0])
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"i":0,"util_pct":85,"mem_used_mb":22528,"mem_total_mb":24576}`; string(b) != want {
			t.Errorf("got %s, want %s", b, want)
		}
	})

	t.Run("garbage and empty input", func(t *testing.T) {
		if gpus := parseNvidiaSMICSV(""); gpus != nil {
			t.Errorf("empty input: got %+v", gpus)
		}
		if gpus := parseNvidiaSMICSV("not,enough\nfields\n"); gpus != nil {
			t.Errorf("garbage input: got %+v", gpus)
		}
	})

	t.Run("unparseable index falls back to row order", func(t *testing.T) {
		out := "[N/A], 10, 100, 200, 40, 50\n[N/A], 20, 100, 200, 40, 50\n"
		gpus := parseNvidiaSMICSV(out)
		if len(gpus) != 2 || gpus[0].Index != 0 || gpus[1].Index != 1 {
			t.Errorf("row-order fallback failed: %+v", gpus)
		}
	})
}
