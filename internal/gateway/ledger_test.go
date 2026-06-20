package gateway

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLedger_RecordAndRecent(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(dir)

	// Record three transactions
	for i := 1; i <= 3; i++ {
		err := l.Record(Transaction{
			Timestamp: time.Now().Add(time.Duration(i) * time.Second),
			Model:     "llama-7b",
			TokensIn:  100 * i,
			TokensOut: 50 * i,
			ACETCost:  i,
			Latency:   float64(100 * i),
			Path:      "/v1/chat/completions",
		})
		if err != nil {
			t.Fatalf("Record(%d): %v", i, err)
		}
	}

	// File should exist
	fp := filepath.Join(dir, "gateway", "transactions.jsonl")
	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("ledger file not found: %v", err)
	}

	// Recent(2) should return 2 items, most-recent first
	recent, err := l.Recent(2)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("Recent(2) returned %d items, want 2", len(recent))
	}
	// Most recent should have ACETCost=3
	if recent[0].ACETCost != 3 {
		t.Errorf("most recent cost = %d, want 3", recent[0].ACETCost)
	}
	if recent[1].ACETCost != 2 {
		t.Errorf("second recent cost = %d, want 2", recent[1].ACETCost)
	}

	// Recent(100) should return all 3
	all, err := l.Recent(100)
	if err != nil {
		t.Fatalf("Recent(100): %v", err)
	}
	if len(all) != 3 {
		t.Errorf("Recent(100) returned %d items, want 3", len(all))
	}
}

func TestLedger_StatsFromDisk(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(dir)

	now := time.Now()
	yesterday := now.Add(-24 * time.Hour)

	// Record a yesterday transaction
	if err := l.Record(Transaction{
		Timestamp: yesterday,
		Model:     "llama-70b",
		TokensIn:  1000,
		TokensOut: 500,
		ACETCost:  10,
		Latency:   200.0,
	}); err != nil {
		t.Fatal(err)
	}

	// Record two today transactions
	for i := 0; i < 2; i++ {
		if err := l.Record(Transaction{
			Timestamp: now,
			Model:     "llama-7b",
			TokensIn:  100,
			TokensOut: 50,
			ACETCost:  5,
			Latency:   100.0,
		}); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := l.StatsFromDisk()
	if err != nil {
		t.Fatalf("StatsFromDisk: %v", err)
	}

	if stats.TotalRequests != 3 {
		t.Errorf("TotalRequests = %d, want 3", stats.TotalRequests)
	}
	if stats.TodayRequests != 2 {
		t.Errorf("TodayRequests = %d, want 2", stats.TodayRequests)
	}

	// Operator share: ceil(80% of cost)
	// Yesterday: ceil(0.8 * 10) = 8
	// Today: 2 * ceil(0.8 * 5) = 2 * 4 = 8
	// Total: 16
	if stats.TotalEarnings != 16 {
		t.Errorf("TotalEarnings = %d, want 16", stats.TotalEarnings)
	}
	if stats.TodayEarnings != 8 {
		t.Errorf("TodayEarnings = %d, want 8", stats.TodayEarnings)
	}

	// Avg latency: (200 + 100 + 100) / 3 ≈ 133.33
	if stats.AvgLatency < 133 || stats.AvgLatency > 134 {
		t.Errorf("AvgLatency = %.2f, want ~133.33", stats.AvgLatency)
	}
}

func TestLedger_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	l := NewLedger(dir)

	recent, err := l.Recent(10)
	if err != nil {
		t.Fatalf("Recent on empty: %v", err)
	}
	if len(recent) != 0 {
		t.Errorf("Recent on empty returned %d, want 0", len(recent))
	}

	stats, err := l.StatsFromDisk()
	if err != nil {
		t.Fatalf("StatsFromDisk on empty: %v", err)
	}
	if stats.TotalRequests != 0 {
		t.Errorf("TotalRequests on empty = %d, want 0", stats.TotalRequests)
	}
}

func TestComputeStats_OperatorCeiling(t *testing.T) {
	// Test that operator share is ceil(80%) — important for small costs
	tests := []struct {
		cost int
		want int // operator share
	}{
		{1, 1},  // ceil(0.8) = 1
		{2, 2},  // ceil(1.6) = 2
		{3, 3},  // ceil(2.4) = 3
		{5, 4},  // ceil(4.0) = 4
		{10, 8}, // ceil(8.0) = 8
	}

	for _, tt := range tests {
		s := computeStats([]Transaction{{
			Timestamp: time.Now(),
			ACETCost:  tt.cost,
			Latency:   100,
		}})
		if s.TotalEarnings != tt.want {
			t.Errorf("cost %d: operator share = %d, want %d", tt.cost, s.TotalEarnings, tt.want)
		}
	}
}
