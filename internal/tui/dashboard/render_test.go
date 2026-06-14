package dashboard

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestStatusModelRender(t *testing.T) {
	data := StatusData{
		NodeName:      "citadel-gpu-01",
		NodeIP:        "100.64.0.42",
		OrgID:         "org_abc123",
		Tags:          []string{"gpu", "vram:24gb"},
		Connected:     true,
		Version:       "2.12.0",
		CPUPercent:    42.3,
		MemoryPercent: 67.8,
		MemoryUsed:    "10.8 GiB",
		MemoryTotal:   "15.9 GiB",
		DiskPercent:   55.2,
		DiskUsed:      "234.5 GiB",
		DiskTotal:     "424.8 GiB",
		GPUs: []GPUInfo{
			{
				Name:        "NVIDIA GeForce RTX 2080",
				Memory:      "7680 MiB / 8192 MiB",
				Temperature: "62",
				Utilization: 35.0,
				Driver:      "535.183.01",
			},
		},
		Services: []ServiceStatus{
			{Name: "vllm", Status: "running", Uptime: "2d 14h"},
			{Name: "ollama", Status: "stopped"},
		},
		Peers: []PeerInfo{
			{Hostname: "macbook-pro", IP: "100.64.0.10", Online: true, Latency: "12ms", ConnType: "direct"},
			{Hostname: "server-02", IP: "100.64.0.15", Online: false},
		},
	}

	model := NewStatusModel(data, nil)

	// Feed a WindowSizeMsg to set dimensions
	updatedModel, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	statusModel := updatedModel.(StatusModel)

	output := statusModel.View()

	// Verify the output is not empty
	if output == "" {
		t.Fatal("View() returned empty string")
	}

	// Verify key content is present
	checks := []string{
		"CITADEL STATUS",
		"citadel-gpu-01",
		"100.64.0.42",
		"SYSTEM VITALS",
		"CPU",
		"Memory",
		"Disk",
		"SERVICES",
		"vllm",
		"ollama",
		"NETWORK PEERS",
		"macbook-pro",
		"server-02",
	}

	for _, check := range checks {
		if !strings.Contains(output, check) {
			t.Errorf("View() output missing expected content: %q", check)
		}
	}

	// Print the output for visual inspection
	t.Logf("Rendered output:\n%s", output)
}

func TestStatusModelRenderDisconnected(t *testing.T) {
	data := StatusData{
		NodeName:      "lonely-node",
		Connected:     false,
		Version:       "2.12.0",
		CPUPercent:    10.0,
		MemoryPercent: 25.0,
		DiskPercent:   30.0,
	}

	model := NewStatusModel(data, nil)
	updatedModel, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	statusModel := updatedModel.(StatusModel)

	output := statusModel.View()

	if !strings.Contains(output, "lonely-node") {
		t.Error("View() output missing node name")
	}
	if !strings.Contains(output, "OFFLINE") {
		t.Error("View() output missing OFFLINE status for disconnected node")
	}
}

func TestStatusModelRenderNoGPU(t *testing.T) {
	data := StatusData{
		NodeName:      "cpu-only-node",
		Connected:     true,
		Version:       "2.12.0",
		CPUPercent:    55.0,
		MemoryPercent: 40.0,
		DiskPercent:   60.0,
	}

	model := NewStatusModel(data, nil)
	updatedModel, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	statusModel := updatedModel.(StatusModel)

	output := statusModel.View()

	if output == "" {
		t.Fatal("View() returned empty string for CPU-only node")
	}
	if !strings.Contains(output, "cpu-only-node") {
		t.Error("View() output missing node name")
	}
}

func TestStatusModelRenderNarrowTerminal(t *testing.T) {
	data := StatusData{
		NodeName:  "narrow-node",
		Connected: true,
		Version:   "2.12.0",
	}

	model := NewStatusModel(data, nil)
	// Test with a narrow terminal (minimum width handling)
	updatedModel, _ := model.Update(tea.WindowSizeMsg{Width: 50, Height: 20})
	statusModel := updatedModel.(StatusModel)

	output := statusModel.View()

	if output == "" {
		t.Fatal("View() returned empty string for narrow terminal")
	}
}

func TestVisibleLength(t *testing.T) {
	tests := []struct {
		input    string
		expected int
	}{
		{"hello", 5},
		{"", 0},
		{"\x1b[31mred\x1b[0m", 3},
		{"\x1b[1;32mbold green\x1b[0m", 10},
	}

	for _, tt := range tests {
		got := visibleLength(tt.input)
		if got != tt.expected {
			t.Errorf("visibleLength(%q) = %d, want %d", tt.input, got, tt.expected)
		}
	}
}
