package status

import (
	"testing"
	"time"
)

func TestNewCollector(t *testing.T) {
	cfg := CollectorConfig{
		NodeName:  "test-node",
		ConfigDir: "/etc/citadel",
		Services: []ServiceConfig{
			{Name: "vllm", Type: "llm", Port: 8000},
		},
	}

	collector := NewCollector(cfg)

	if collector == nil {
		t.Fatal("NewCollector returned nil")
	}
	if collector.nodeName != "test-node" {
		t.Errorf("nodeName = %v, want test-node", collector.nodeName)
	}
	if collector.configDir != "/etc/citadel" {
		t.Errorf("configDir = %v, want /etc/citadel", collector.configDir)
	}
	if len(collector.services) != 1 {
		t.Errorf("services count = %v, want 1", len(collector.services))
	}
	if collector.startTime.IsZero() {
		t.Error("startTime should not be zero")
	}
}

func TestCollectorCollect(t *testing.T) {
	collector := NewCollector(CollectorConfig{
		NodeName: "test-node",
	})

	status, err := collector.Collect()

	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if status == nil {
		t.Fatal("Collect() returned nil status")
	}

	// Check version
	if status.Version != StatusVersion {
		t.Errorf("Version = %v, want %v", status.Version, StatusVersion)
	}

	// Check timestamp
	if status.Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	}

	// Check node info
	if status.Node.Name != "test-node" {
		t.Errorf("Node.Name = %v, want test-node", status.Node.Name)
	}
	if status.Node.UptimeSeconds < 0 {
		t.Errorf("Node.UptimeSeconds = %v, should be >= 0", status.Node.UptimeSeconds)
	}

	// Check system metrics exist (values may vary)
	// CPUPercent should be between 0 and 100
	if status.System.CPUPercent < 0 || status.System.CPUPercent > 100 {
		t.Errorf("System.CPUPercent = %v, should be 0-100", status.System.CPUPercent)
	}
}

func TestCollectorCollectCompact(t *testing.T) {
	collector := NewCollector(CollectorConfig{
		NodeName: "test-node",
	})

	status, err := collector.CollectCompact()

	if err != nil {
		t.Fatalf("CollectCompact() error = %v", err)
	}
	if status == nil {
		t.Fatal("CollectCompact() returned nil status")
	}

	// Should have same data as full collect
	if status.Node.Name != "test-node" {
		t.Errorf("Node.Name = %v, want test-node", status.Node.Name)
	}
}

func TestCollectorUptime(t *testing.T) {
	collector := NewCollector(CollectorConfig{
		NodeName: "test-node",
	})

	// Wait a bit
	time.Sleep(10 * time.Millisecond)

	status, _ := collector.Collect()

	if status.Node.UptimeSeconds < 0 {
		t.Error("UptimeSeconds should be positive after waiting")
	}
}

func TestServiceConfig(t *testing.T) {
	svc := ServiceConfig{
		Name:        "vllm",
		Type:        "llm",
		ComposeFile: "vllm.yml",
		Port:        8000,
	}

	if svc.Name != "vllm" {
		t.Errorf("Name = %v, want vllm", svc.Name)
	}
	if svc.Type != "llm" {
		t.Errorf("Type = %v, want llm", svc.Type)
	}
	if svc.Port != 8000 {
		t.Errorf("Port = %v, want 8000", svc.Port)
	}
}

func TestCollectorConfig(t *testing.T) {
	cfg := CollectorConfig{
		NodeName:  "my-node",
		ConfigDir: "/home/user/citadel",
		Services: []ServiceConfig{
			{Name: "ollama", Type: ServiceTypeLLM, Port: 11434},
			{Name: "vllm", Type: ServiceTypeLLM, Port: 8000},
		},
	}

	if cfg.NodeName != "my-node" {
		t.Errorf("NodeName = %v, want my-node", cfg.NodeName)
	}
	if len(cfg.Services) != 2 {
		t.Errorf("Services count = %v, want 2", len(cfg.Services))
	}
}

// TestCollectorWorkerLiveness verifies the #548 heartbeat liveness attachment:
// when a WorkerLiveness provider is set, Collect() attaches it; when it is not,
// the field is omitted so the payload stays back-compatible for non-worker nodes.
func TestCollectorWorkerLiveness(t *testing.T) {
	consumedAt := time.Now().Add(-time.Minute)

	withFn := NewCollector(CollectorConfig{
		NodeName: "test-node",
		WorkerLiveness: func() *WorkerLiveness {
			return &WorkerLiveness{
				Consuming:         true,
				LastJobConsumedAt: &consumedAt,
				InFlight:          2,
			}
		},
	})
	st, err := withFn.Collect()
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if st.Worker == nil {
		t.Fatal("Worker liveness not attached when provider was set")
	}
	if !st.Worker.Consuming || st.Worker.InFlight != 2 || st.Worker.LastJobConsumedAt == nil {
		t.Errorf("Worker = %+v, want consuming=true in_flight=2 with a consumed-at time", st.Worker)
	}

	without := NewCollector(CollectorConfig{NodeName: "test-node"})
	st2, err := without.Collect()
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if st2.Worker != nil {
		t.Errorf("Worker = %+v, want nil (omitted) when no provider set", st2.Worker)
	}
}

// TestCollectorSwapActivity verifies the #717 heartbeat swap-activity
// attachment: when a SwapStats provider is set, Collect() attaches it; when it
// is not (hotswap off, or a node with no swap manager wired at all), the field
// is omitted so a hotswap-off heartbeat is byte-identical to one predating this
// field. Mirrors TestCollectorWorkerLiveness above.
func TestCollectorSwapActivity(t *testing.T) {
	withFn := NewCollector(CollectorConfig{
		NodeName: "test-node",
		SwapStats: func() *SwapActivity {
			return &SwapActivity{
				SwapsPerHour:         3,
				EvictingSwapsPerHour: 1,
				MaxEvictingPerHour:   6,
				Recent: []SwapRecord{
					{Backend: "bonsai", Model: "Bonsai-27B-Q1_0.gguf", Outcome: "ready"},
				},
			}
		},
	})
	st, err := withFn.Collect()
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if st.Swap == nil {
		t.Fatal("Swap activity not attached when provider was set")
	}
	if st.Swap.SwapsPerHour != 3 || st.Swap.EvictingSwapsPerHour != 1 || st.Swap.MaxEvictingPerHour != 6 {
		t.Errorf("Swap = %+v, want swaps_per_hour=3 evicting=1 max=6", st.Swap)
	}
	if len(st.Swap.Recent) != 1 || st.Swap.Recent[0].Backend != "bonsai" {
		t.Errorf("Swap.Recent = %+v, want one bonsai record", st.Swap.Recent)
	}

	without := NewCollector(CollectorConfig{NodeName: "test-node"})
	st2, err := without.Collect()
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if st2.Swap != nil {
		t.Errorf("Swap = %+v, want nil (omitted) when no provider set", st2.Swap)
	}
}

// TestCollectorReconcileHealth verifies the citadel-cli#742 heartbeat
// attachment: when the ReconcileHealth provider is set AND currently
// reports a refusal, Collect() attaches it; when the provider is unset, OR
// set but reporting the healthy/never-refused state, the field is omitted --
// the deliberate asymmetry with WorkerLiveness/SwapActivity above (which are
// always attached once wired, healthy or not): NodeStatus.Reconcile is
// present ONLY for the alarm, so a healthy node's payload -- with or without
// a reconcile loop running at all -- stays unchanged.
func TestCollectorReconcileHealth(t *testing.T) {
	since := time.Now().Add(-30 * time.Minute)
	withRefusal := NewCollector(CollectorConfig{
		NodeName: "test-node",
		ReconcileHealth: func() *ReconcileHealth {
			return &ReconcileHealth{Refused: true, Reason: "refusing empty desired state", Since: since, Count: 6}
		},
	})
	st, err := withRefusal.Collect()
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if st.Reconcile == nil {
		t.Fatal("Reconcile health not attached when provider reported a refusal")
	}
	if !st.Reconcile.Refused || st.Reconcile.Count != 6 || st.Reconcile.Reason == "" {
		t.Errorf("Reconcile = %+v, want refused=true count=6 with a reason", st.Reconcile)
	}

	// Provider wired but currently healthy (returns nil): must stay omitted.
	healthy := NewCollector(CollectorConfig{
		NodeName:        "test-node",
		ReconcileHealth: func() *ReconcileHealth { return nil },
	})
	stHealthy, err := healthy.Collect()
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if stHealthy.Reconcile != nil {
		t.Errorf("Reconcile = %+v, want nil (omitted) when the provider reports healthy", stHealthy.Reconcile)
	}

	// No provider at all (no reconcile loop wired): must stay omitted, same
	// payload shape as every node predating this field.
	without := NewCollector(CollectorConfig{NodeName: "test-node"})
	st2, err := without.Collect()
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if st2.Reconcile != nil {
		t.Errorf("Reconcile = %+v, want nil (omitted) when no provider set", st2.Reconcile)
	}
}
