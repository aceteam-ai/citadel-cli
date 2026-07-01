package footprint

import (
	"math"
	"testing"
	"time"
)

// writeSyntheticCSV appends samples to a store so the query path reads real
// CSVs (exercising header-skipping + parsing), not in-memory structs.
func writeSyntheticCSV(t *testing.T, dir string, samples []Sample) {
	t.Helper()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	// Group by day so Append (which files by the first sample's day) rotates
	// correctly for multi-day inputs.
	byDay := map[string][]Sample{}
	for _, s := range samples {
		key := s.Timestamp.UTC().Format("2006-01-02")
		byDay[key] = append(byDay[key], s)
	}
	for _, batch := range byDay {
		if err := store.Append(batch); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
}

func TestSummarizeAvgPeakAndIdle(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	interval := time.Minute

	// vllm: RSS 1000, 2000, 3000 -> avg 2000 peak 3000; CPU 10,20,30 -> avg 20 peak 30.
	// idle_seconds present on samples 2 and 3 (idle), sample 1 active.
	samples := []Sample{
		{Timestamp: base, NodeID: "n", Service: "vllm", Running: true, RSSMB: f64(1000), CPUPercent: f64(10)},
		{Timestamp: base.Add(interval), NodeID: "n", Service: "vllm", Running: true, RSSMB: f64(2000), CPUPercent: f64(20), IdleSeconds: ip(60)},
		{Timestamp: base.Add(2 * interval), NodeID: "n", Service: "vllm", Running: true, RSSMB: f64(3000), CPUPercent: f64(30), IdleSeconds: ip(120)},
		// node-level row with VRAM.
		{Timestamp: base, NodeID: "n", Service: NodeService, Running: true, VRAMMB: ip(8000), GPUUtilPercent: f64(50)},
	}
	writeSyntheticCSV(t, dir, samples)

	got, err := Summarize(dir, QueryOptions{Now: base.Add(3 * interval)}, interval)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	// Sorted: NodeService "_node" sorts before "vllm".
	if len(got) != 2 {
		t.Fatalf("expected 2 service summaries, got %d: %+v", len(got), got)
	}

	var vllm *ServiceSummary
	for i := range got {
		if got[i].Service == "vllm" {
			vllm = &got[i]
		}
	}
	if vllm == nil {
		t.Fatal("missing vllm summary")
	}
	if vllm.Samples != 3 {
		t.Errorf("vllm samples = %d, want 3", vllm.Samples)
	}
	if math.Abs(vllm.AvgRSSMB-2000) > 1e-6 || vllm.PeakRSSMB != 3000 {
		t.Errorf("vllm RSS avg/peak = %.1f/%.1f, want 2000/3000", vllm.AvgRSSMB, vllm.PeakRSSMB)
	}
	if math.Abs(vllm.AvgCPUPercent-20) > 1e-6 || vllm.PeakCPUPercent != 30 {
		t.Errorf("vllm CPU avg/peak = %.1f/%.1f, want 20/30", vllm.AvgCPUPercent, vllm.PeakCPUPercent)
	}
	// 2 of 3 samples idle -> 66.67%.
	if math.Abs(vllm.IdlePercent-(2.0/3.0*100)) > 0.1 {
		t.Errorf("vllm idle%% = %.2f, want ~66.67", vllm.IdlePercent)
	}
	// Longest idle stretch: samples 2 and 3 are consecutive idle -> run length 2 -> 2*interval.
	if vllm.LongestIdle != 2*interval {
		t.Errorf("vllm longest idle = %s, want %s", vllm.LongestIdle, 2*interval)
	}
}

func TestSummarizeLongestIdleStretchResetsOnActive(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	interval := time.Minute
	// idle pattern: I I A I I I A -> longest run is 3.
	idle := []bool{true, true, false, true, true, true, false}
	var samples []Sample
	for i, isIdle := range idle {
		s := Sample{Timestamp: base.Add(time.Duration(i) * interval), NodeID: "n", Service: "svc", Running: true, RSSMB: f64(100)}
		if isIdle {
			s.IdleSeconds = ip(30)
		} else {
			s.IdleSeconds = ip(0)
		}
		samples = append(samples, s)
	}
	writeSyntheticCSV(t, dir, samples)

	got, err := Summarize(dir, QueryOptions{Now: base.Add(10 * interval)}, interval)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(got))
	}
	if got[0].LongestIdle != 3*interval {
		t.Errorf("longest idle = %s, want %s", got[0].LongestIdle, 3*interval)
	}
}

func TestSummarizeSinceFilter(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	interval := time.Minute
	samples := []Sample{
		{Timestamp: now.AddDate(0, 0, -1), NodeID: "n", Service: "svc", Running: true, RSSMB: f64(999)}, // old
		{Timestamp: now.Add(-30 * time.Minute), NodeID: "n", Service: "svc", Running: true, RSSMB: f64(100)},
	}
	writeSyntheticCSV(t, dir, samples)

	got, err := Summarize(dir, QueryOptions{Since: time.Hour, Now: now}, interval)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Samples != 1 {
		t.Fatalf("since filter should keep 1 recent sample, got %+v", got)
	}
	if got[0].PeakRSSMB != 100 {
		t.Errorf("expected only recent sample (100), got peak %.0f", got[0].PeakRSSMB)
	}
}

func TestSummarizeServiceAndIdleOnlyFilters(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	samples := []Sample{
		{Timestamp: base, NodeID: "n", Service: "a", Running: true, RSSMB: f64(1), IdleSeconds: ip(0)},
		{Timestamp: base, NodeID: "n", Service: "a", Running: true, RSSMB: f64(2), IdleSeconds: ip(10)},
		{Timestamp: base, NodeID: "n", Service: "b", Running: true, RSSMB: f64(3), IdleSeconds: ip(10)},
	}
	writeSyntheticCSV(t, dir, samples)

	// Service filter.
	got, _ := Summarize(dir, QueryOptions{Service: "a"}, time.Minute)
	if len(got) != 1 || got[0].Service != "a" || got[0].Samples != 2 {
		t.Fatalf("service filter failed: %+v", got)
	}
	// Idle-only filter on service a -> only the idle sample remains.
	got, _ = Summarize(dir, QueryOptions{Service: "a", IdleOnly: true}, time.Minute)
	if len(got) != 1 || got[0].Samples != 1 {
		t.Fatalf("idle-only filter failed: %+v", got)
	}
}

func TestSummarizeMissingDirIsEmpty(t *testing.T) {
	got, err := Summarize("/nonexistent/footprints/dir", QueryOptions{}, time.Minute)
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %+v", got)
	}
}
