package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestCalculateACETCost(t *testing.T) {
	tests := []struct {
		tier     string
		in, out  int
		wantCost int
	}{
		// Small tier: 1 ACET per 1K tokens
		{"small", 500, 500, 1},   // 1000 tokens = 1 ACET
		{"small", 100, 50, 1},    // 150 tokens = ceil(0.15) = 1
		{"small", 2000, 1000, 3}, // 3000 tokens = 3 ACET

		// Medium tier: 5 ACET per 1K tokens
		{"medium", 1000, 500, 8}, // 1500 tokens = ceil(7.5) = 8
		{"medium", 100, 100, 1},  // 200 tokens = ceil(1.0) = 1

		// Large tier: 25 ACET per 1K tokens
		{"large", 1000, 500, 38}, // 1500 tokens = ceil(37.5) = 38
		{"large", 100, 0, 3},     // 100 tokens = ceil(2.5) = 3

		// XLarge tier: 100 ACET per 1K tokens
		{"xlarge", 1000, 1000, 200}, // 2000 tokens = 200 ACET
		{"xlarge", 10, 10, 2},       // 20 tokens = ceil(2.0) = 2
	}

	for _, tt := range tests {
		tier, ok := TierByName(tt.tier)
		if !ok {
			t.Fatalf("unknown tier: %s", tt.tier)
		}
		got := CalculateACETCost(tier, tt.in, tt.out)
		if got != tt.wantCost {
			t.Errorf("%s(%d in, %d out) = %d ACET, want %d",
				tt.tier, tt.in, tt.out, got, tt.wantCost)
		}
	}
}

func TestOperatorPlatformSplit(t *testing.T) {
	tests := []struct {
		cost         int
		wantOperator int
		wantPlatform int
	}{
		{1, 1, 0},  // ceil(0.8) = 1
		{2, 2, 0},  // ceil(1.6) = 2
		{3, 3, 0},  // ceil(2.4) = 3
		{5, 4, 1},  // ceil(4.0) = 4
		{10, 8, 2}, // ceil(8.0) = 8
		{100, 80, 20},
	}

	for _, tt := range tests {
		op := OperatorShare(tt.cost)
		pl := PlatformShare(tt.cost)
		if op != tt.wantOperator {
			t.Errorf("OperatorShare(%d) = %d, want %d", tt.cost, op, tt.wantOperator)
		}
		if pl != tt.wantPlatform {
			t.Errorf("PlatformShare(%d) = %d, want %d", tt.cost, pl, tt.wantPlatform)
		}
		if op+pl != tt.cost {
			t.Errorf("shares don't sum: %d + %d != %d", op, pl, tt.cost)
		}
	}
}

func TestACETClient_Settle(t *testing.T) {
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)

		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/acet/settle" {
			t.Errorf("path = %s, want /acet/settle", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("auth = %q, want 'Bearer test-token'", r.Header.Get("Authorization"))
		}

		var body settleRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body.Model != "llama-7b" {
			t.Errorf("model = %s, want llama-7b", body.Model)
		}
		if body.ACETCost != 5 {
			t.Errorf("cost = %d, want 5", body.ACETCost)
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewACETClient(server.URL, "test-token")
	err := client.Settle("llama-7b", 1000, 500, 5, "org-123")
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}

	if atomic.LoadInt32(&callCount) != 1 {
		t.Errorf("settle called %d times, want 1", callCount)
	}
}

func TestACETClient_SettleOfflineQueue(t *testing.T) {
	var callCount int32
	failFirst := true

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		if failFirst {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewACETClient(server.URL, "test-token")

	// First settle should fail and queue
	err := client.Settle("llama-7b", 100, 50, 1, "org-1")
	if err == nil {
		t.Fatal("expected error from offline settle")
	}

	if client.QueueLen() != 1 {
		t.Errorf("queue len = %d, want 1", client.QueueLen())
	}

	// Now allow settlements
	failFirst = false
	settled, remaining := client.FlushOfflineQueue()
	if settled != 1 {
		t.Errorf("settled = %d, want 1", settled)
	}
	if remaining != 0 {
		t.Errorf("remaining = %d, want 0", remaining)
	}
	if client.QueueLen() != 0 {
		t.Errorf("queue len after flush = %d, want 0", client.QueueLen())
	}
}

func TestACETClient_CheckBalance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/acet/balance" {
			t.Errorf("path = %s, want /acet/balance", r.URL.Path)
		}
		if r.URL.Query().Get("org") != "org-42" {
			t.Errorf("org = %s, want org-42", r.URL.Query().Get("org"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(balanceResponse{Balance: 1500, OK: true})
	}))
	defer server.Close()

	client := NewACETClient(server.URL, "")
	balance, err := client.CheckBalance("org-42")
	if err != nil {
		t.Fatalf("CheckBalance: %v", err)
	}
	if balance != 1500 {
		t.Errorf("balance = %d, want 1500", balance)
	}
}

func TestTierByName(t *testing.T) {
	for _, name := range []string{"small", "medium", "large", "xlarge"} {
		tier, ok := TierByName(name)
		if !ok {
			t.Errorf("TierByName(%q) not found", name)
		}
		if tier.Name != name {
			t.Errorf("tier.Name = %q, want %q", tier.Name, name)
		}
	}

	_, ok := TierByName("nonexistent")
	if ok {
		t.Error("TierByName('nonexistent') should return false")
	}
}

func TestDefaultPricingTiers(t *testing.T) {
	tiers := DefaultPricingTiers()
	if len(tiers) != 4 {
		t.Fatalf("got %d tiers, want 4", len(tiers))
	}

	// Verify tiers are ordered by model size
	expected := []struct {
		name  string
		per1K int
	}{
		{"small", 1},
		{"medium", 5},
		{"large", 25},
		{"xlarge", 100},
	}

	for i, e := range expected {
		if tiers[i].Name != e.name || tiers[i].ACETPer1K != e.per1K {
			t.Errorf("tier[%d] = {%s, %d}, want {%s, %d}",
				i, tiers[i].Name, tiers[i].ACETPer1K, e.name, e.per1K)
		}
	}
}
