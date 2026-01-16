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
