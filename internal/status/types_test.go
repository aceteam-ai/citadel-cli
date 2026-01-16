package status

import (
	"testing"
	"time"
)

func TestStatusVersion(t *testing.T) {
	if StatusVersion == "" {
		t.Error("StatusVersion should not be empty")
	}
	if StatusVersion != "1.0" {
		t.Errorf("StatusVersion = %v, want 1.0", StatusVersion)
	}
}

func TestHealthStatusConstants(t *testing.T) {
	tests := []struct {
		status string
		want   string
	}{
		{HealthStatusOK, "ok"},
		{HealthStatusDegraded, "degraded"},
		{HealthStatusUnhealthy, "unhealthy"},
		{HealthStatusUnknown, "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			if tt.status != tt.want {
				t.Errorf("HealthStatus = %v, want %v", tt.status, tt.want)
			}
		})
	}
}

func TestServiceStatusConstants(t *testing.T) {
	if ServiceStatusRunning != "running" {
		t.Errorf("ServiceStatusRunning = %v, want running", ServiceStatusRunning)
	}
	if ServiceStatusStopped != "stopped" {
		t.Errorf("ServiceStatusStopped = %v, want stopped", ServiceStatusStopped)
	}
	if ServiceStatusError != "error" {
		t.Errorf("ServiceStatusError = %v, want error", ServiceStatusError)
	}
}

func TestServiceTypeConstants(t *testing.T) {
	if ServiceTypeLLM != "llm" {
		t.Errorf("ServiceTypeLLM = %v, want llm", ServiceTypeLLM)
	}
	if ServiceTypeDatabase != "database" {
		t.Errorf("ServiceTypeDatabase = %v, want database", ServiceTypeDatabase)
	}
	if ServiceTypeOther != "other" {
		t.Errorf("ServiceTypeOther = %v, want other", ServiceTypeOther)
	}
}

func TestNodeStatus(t *testing.T) {
	status := &NodeStatus{
		Version:   "1.0",
		Timestamp: time.Now(),
		Node: NodeInfo{
			Name:          "test-node",
			TailscaleIP:   "100.64.0.1",
			UptimeSeconds: 3600,
		},
		System: SystemMetrics{
			CPUPercent:    50.0,
			MemoryPercent: 60.0,
			MemoryTotalGB: 16.0,
			MemoryUsedGB:  9.6,
			DiskPercent:   70.0,
			DiskTotalGB:   500.0,
			DiskUsedGB:    350.0,
		},
		GPU: []GPUMetrics{
			{
				Index:              0,
				Name:               "NVIDIA RTX 4090",
				MemoryTotalMB:      24576,
				MemoryUsedMB:       8192,
				UtilizationPercent: 75,
				TemperatureCelsius: 65,
			},
		},
		Services: []ServiceInfo{
			{
				Name:   "vllm",
				Type:   ServiceTypeLLM,
				Status: ServiceStatusRunning,
				Port:   8000,
				Health: HealthStatusOK,
			},
		},
	}

	if status.Version != "1.0" {
		t.Errorf("Version = %v, want 1.0", status.Version)
	}
	if status.Node.Name != "test-node" {
		t.Errorf("Node.Name = %v, want test-node", status.Node.Name)
	}
	if status.System.CPUPercent != 50.0 {
		t.Errorf("System.CPUPercent = %v, want 50.0", status.System.CPUPercent)
	}
	if len(status.GPU) != 1 {
		t.Errorf("GPU count = %v, want 1", len(status.GPU))
	}
	if len(status.Services) != 1 {
		t.Errorf("Services count = %v, want 1", len(status.Services))
	}
}

func TestHealthResponse(t *testing.T) {
	resp := HealthResponse{
		Status:  HealthStatusOK,
		Version: "1.2.3",
	}

	if resp.Status != HealthStatusOK {
		t.Errorf("Status = %v, want %v", resp.Status, HealthStatusOK)
	}
	if resp.Version != "1.2.3" {
		t.Errorf("Version = %v, want 1.2.3", resp.Version)
	}
}

func TestGPUMetrics(t *testing.T) {
	gpu := GPUMetrics{
		Index:              0,
		Name:               "NVIDIA A100",
		MemoryTotalMB:      40960,
		MemoryUsedMB:       20480,
		UtilizationPercent: 80,
		TemperatureCelsius: 70,
		Driver:             "535.86.10",
	}

	if gpu.Index != 0 {
		t.Errorf("Index = %v, want 0", gpu.Index)
	}
	if gpu.Name != "NVIDIA A100" {
		t.Errorf("Name = %v, want NVIDIA A100", gpu.Name)
	}
	if gpu.MemoryTotalMB != 40960 {
		t.Errorf("MemoryTotalMB = %v, want 40960", gpu.MemoryTotalMB)
	}
}

func TestServiceInfo(t *testing.T) {
	svc := ServiceInfo{
		Name:   "ollama",
		Type:   ServiceTypeLLM,
		Status: ServiceStatusRunning,
		Port:   11434,
		Health: HealthStatusOK,
		Models: []string{"llama2", "mistral"},
	}

	if svc.Name != "ollama" {
		t.Errorf("Name = %v, want ollama", svc.Name)
	}
	if svc.Status != ServiceStatusRunning {
		t.Errorf("Status = %v, want %v", svc.Status, ServiceStatusRunning)
	}
	if svc.Port != 11434 {
		t.Errorf("Port = %v, want 11434", svc.Port)
	}
	if len(svc.Models) != 2 {
		t.Errorf("Models count = %v, want 2", len(svc.Models))
	}
}

func TestNodeInfo(t *testing.T) {
	info := NodeInfo{
		Name:          "gpu-node-1",
		TailscaleIP:   "100.64.0.10",
		UptimeSeconds: 86400,
	}

	if info.Name != "gpu-node-1" {
		t.Errorf("Name = %v, want gpu-node-1", info.Name)
	}
	if info.TailscaleIP != "100.64.0.10" {
		t.Errorf("TailscaleIP = %v, want 100.64.0.10", info.TailscaleIP)
	}
	if info.UptimeSeconds != 86400 {
		t.Errorf("UptimeSeconds = %v, want 86400", info.UptimeSeconds)
	}
}

func TestSystemMetrics(t *testing.T) {
	sys := SystemMetrics{
		CPUPercent:    25.5,
		MemoryUsedGB:  8.0,
		MemoryTotalGB: 32.0,
		MemoryPercent: 25.0,
		DiskUsedGB:    100.0,
		DiskTotalGB:   500.0,
		DiskPercent:   20.0,
	}

	if sys.CPUPercent != 25.5 {
		t.Errorf("CPUPercent = %v, want 25.5", sys.CPUPercent)
	}
	if sys.MemoryPercent != 25.0 {
		t.Errorf("MemoryPercent = %v, want 25.0", sys.MemoryPercent)
	}
	if sys.DiskPercent != 20.0 {
		t.Errorf("DiskPercent = %v, want 20.0", sys.DiskPercent)
	}
}
