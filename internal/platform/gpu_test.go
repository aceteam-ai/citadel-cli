package platform

import (
	"runtime"
	"testing"
)

func TestGetGPUDetector(t *testing.T) {
	detector, err := GetGPUDetector()
	if err != nil {
		t.Fatalf("GetGPUDetector() error = %v", err)
	}

	if detector == nil {
		t.Fatal("GetGPUDetector() returned nil")
	}

	// Verify correct detector for platform
	switch runtime.GOOS {
	case "linux":
		_, ok := detector.(*LinuxGPUDetector)
		if !ok {
			t.Errorf("GetGPUDetector() on Linux did not return LinuxGPUDetector")
		}
	case "darwin":
		_, ok := detector.(*DarwinGPUDetector)
		if !ok {
			t.Errorf("GetGPUDetector() on macOS did not return DarwinGPUDetector")
		}
	case "windows":
		_, ok := detector.(*WindowsGPUDetector)
		if !ok {
			t.Errorf("GetGPUDetector() on Windows did not return WindowsGPUDetector")
		}
	}
}

func TestWindowsGPUDetectorGetGPUCount(t *testing.T) {
	if !IsWindows() {
		t.Skip("Skipping Windows-specific test")
	}

	detector := &WindowsGPUDetector{}

	// This should not panic even if no GPU is present
	count := detector.GetGPUCount()
	if count < 0 {
		t.Errorf("WindowsGPUDetector.GetGPUCount() = %d, want >= 0", count)
	}
}

func TestFormatGPUInfo(t *testing.T) {
	// Test with empty slice
	result := FormatGPUInfo([]GPUInfo{})
	if result != "No GPU detected" {
		t.Errorf("FormatGPUInfo(empty) = %s, want 'No GPU detected'", result)
	}

	// Test with sample GPU info
	gpus := []GPUInfo{
		{
			Name:        "NVIDIA GeForce RTX 4090",
			Memory:      "24576 MB",
			Temperature: "45°C",
			Utilization: "10%",
			Driver:      "535.54.03",
		},
	}

	result = FormatGPUInfo(gpus)
	if result == "" {
		t.Error("FormatGPUInfo() returned empty string for valid GPU")
	}

	// Should contain GPU name
	if !contains(result, "RTX 4090") {
		t.Errorf("FormatGPUInfo() = %s, should contain 'RTX 4090'", result)
	}
}
