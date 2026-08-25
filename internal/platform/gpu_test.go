package platform

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
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

func TestNvidiaSMIExitHint(t *testing.T) {
	tests := []struct {
		code     int
		wantHint string
	}{
		{6, "No NVIDIA GPU detected by the driver"},
		{9, "GPU hardware error"},
		{15, "Driver version mismatch"},
		{18, "NVIDIA drivers not loaded"},
		{1, ""},  // unknown code
		{0, ""},  // success (shouldn't be called, but safe)
		{42, ""}, // arbitrary unknown code
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("exit_%d", tt.code), func(t *testing.T) {
			hint := NvidiaSMIExitHint(tt.code)
			if tt.wantHint == "" {
				if hint != "" {
					t.Errorf("NvidiaSMIExitHint(%d) = %q, want empty", tt.code, hint)
				}
			} else {
				if !strings.Contains(hint, tt.wantHint) {
					t.Errorf("NvidiaSMIExitHint(%d) = %q, want substring %q", tt.code, hint, tt.wantHint)
				}
			}
		})
	}
}

func TestNvidiaSMIErrorMessage(t *testing.T) {
	// nil error
	if msg := NvidiaSMIErrorMessage(nil); msg != "" {
		t.Errorf("NvidiaSMIErrorMessage(nil) = %q, want empty", msg)
	}

	// Binary not found (exec.Error)
	pathErr := &exec.Error{Name: "nvidia-smi", Err: exec.ErrNotFound}
	msg := NvidiaSMIErrorMessage(pathErr)
	if !strings.Contains(msg, "not found") && !strings.Contains(msg, "not installed") {
		t.Errorf("NvidiaSMIErrorMessage(pathErr) = %q, want 'not found' or 'not installed'", msg)
	}

	// Known exit code — produce a real *exec.ExitError via sh
	if runtime.GOOS != "windows" {
		cmd := exec.Command("sh", "-c", "exit 18")
		err := cmd.Run()
		if err == nil {
			t.Fatal("expected exit 18 to produce an error")
		}
		msg = NvidiaSMIErrorMessage(err)
		if !strings.Contains(msg, "NVIDIA drivers not loaded") {
			t.Errorf("NvidiaSMIErrorMessage(exit 18) = %q, want 'NVIDIA drivers not loaded'", msg)
		}
	}

	// Unknown exit code
	if runtime.GOOS != "windows" {
		cmd := exec.Command("sh", "-c", "exit 99")
		err := cmd.Run()
		if err == nil {
			t.Fatal("expected exit 99 to produce an error")
		}
		msg = NvidiaSMIErrorMessage(err)
		if !strings.Contains(msg, "99") {
			t.Errorf("NvidiaSMIErrorMessage(exit 99) = %q, want to contain '99'", msg)
		}
	}

	// Wrapped error — simulates what GetGPUInfo returns: fmt.Errorf("failed to query...: %w", exitErr)
	if runtime.GOOS != "windows" {
		cmd := exec.Command("sh", "-c", "exit 18")
		err := cmd.Run()
		if err == nil {
			t.Fatal("expected exit 18 to produce an error")
		}
		wrapped := fmt.Errorf("failed to query NVIDIA GPUs: %w", err)
		msg = NvidiaSMIErrorMessage(wrapped)
		if !strings.Contains(msg, "NVIDIA drivers not loaded") {
			t.Errorf("NvidiaSMIErrorMessage(wrapped exit 18) = %q, want 'NVIDIA drivers not loaded'", msg)
		}
	}

	// Arbitrary error (neither exec.Error nor exec.ExitError)
	genericErr := errors.New("something went wrong")
	msg = NvidiaSMIErrorMessage(genericErr)
	if !strings.Contains(msg, "something went wrong") {
		t.Errorf("NvidiaSMIErrorMessage(generic) = %q, want to contain original message", msg)
	}
}

func TestNvidiaSMIErrorMessageStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping on Windows")
	}

	// Driver/library version mismatch in stderr → reboot message
	t.Run("driver_library_mismatch", func(t *testing.T) {
		_, err := exec.Command("sh", "-c",
			"echo 'Failed to initialize NVML: Driver/library version mismatch' >&2; exit 18").Output()
		if err == nil {
			t.Fatal("expected error")
		}
		msg := NvidiaSMIErrorMessage(err)
		if !strings.Contains(msg, "Driver/library version mismatch") {
			t.Errorf("want 'Driver/library version mismatch', got %q", msg)
		}
		if !strings.Contains(msg, "reboot required") {
			t.Errorf("want 'reboot required', got %q", msg)
		}
	})

	// NVML Shared Library Not Found → drivers not installed
	t.Run("nvml_not_found", func(t *testing.T) {
		_, err := exec.Command("sh", "-c",
			"echo 'NVML Shared Library Not Found' >&2; exit 1").Output()
		if err == nil {
			t.Fatal("expected error")
		}
		msg := NvidiaSMIErrorMessage(err)
		if !strings.Contains(msg, "not installed") {
			t.Errorf("want 'not installed', got %q", msg)
		}
	})

	// Unknown stderr → surface stderr text
	t.Run("unknown_stderr", func(t *testing.T) {
		_, err := exec.Command("sh", "-c",
			"echo 'Some unexpected NVML error' >&2; exit 42").Output()
		if err == nil {
			t.Fatal("expected error")
		}
		msg := NvidiaSMIErrorMessage(err)
		if !strings.Contains(msg, "Some unexpected NVML error") {
			t.Errorf("want stderr text in message, got %q", msg)
		}
	})

	// Wrapped error with stderr (simulates GetGPUInfo path)
	t.Run("wrapped_with_stderr", func(t *testing.T) {
		_, err := exec.Command("sh", "-c",
			"echo 'Failed to initialize NVML: Driver/library version mismatch' >&2; exit 18").Output()
		if err == nil {
			t.Fatal("expected error")
		}
		wrapped := fmt.Errorf("failed to query NVIDIA GPUs: %w", err)
		msg := NvidiaSMIErrorMessage(wrapped)
		if !strings.Contains(msg, "reboot required") {
			t.Errorf("want 'reboot required' through wrapper, got %q", msg)
		}
	})
}

func TestParseNvidiaSMICSVLine(t *testing.T) {
	// name, memory.total, memory.used, temperature.gpu, utilization.gpu,
	// driver_version, memory.free — csv,noheader,nounits.
	line := "NVIDIA GeForce RTX 3090, 24576, 17827, 65, 12, 535.86.10, 6292"
	gpu, ok := parseNvidiaSMICSVLine(line)
	if !ok {
		t.Fatal("expected a valid parse")
	}
	if gpu.Name != "NVIDIA GeForce RTX 3090" {
		t.Errorf("Name = %q", gpu.Name)
	}
	if gpu.Memory != "24576 MB" {
		t.Errorf("Memory = %q, want '24576 MB'", gpu.Memory)
	}
	if gpu.MemoryUsed != "17827 MB" {
		t.Errorf("MemoryUsed = %q, want '17827 MB'", gpu.MemoryUsed)
	}
	// The whole point of probing memory.free directly (citadel #833): it is
	// NOT total-used (24576-17827=6749) — nvidia-smi reserves memory that
	// counts against neither, so a derived value would overstate what's free.
	if gpu.MemoryFree != "6292 MB" {
		t.Errorf("MemoryFree = %q, want '6292 MB' (not the derived 6749)", gpu.MemoryFree)
	}
	if gpu.Temperature != "65°C" {
		t.Errorf("Temperature = %q", gpu.Temperature)
	}
	if gpu.Utilization != "12%" {
		t.Errorf("Utilization = %q", gpu.Utilization)
	}
	if gpu.Driver != "535.86.10" {
		t.Errorf("Driver = %q", gpu.Driver)
	}
}

func TestParseNvidiaSMICSVLine_MissingMemoryFreeDegradesNotFails(t *testing.T) {
	// A pre-#833 6-field row (no memory.free) must still parse: GPU detection
	// (name/memory/temp/util/driver) backs capability detection and
	// inference-queue routing, so it must not go all-or-nothing on one new
	// optional field a driver/GPU combination might someday omit.
	gpu, ok := parseNvidiaSMICSVLine("RTX 3090, 24576, 17827, 65, 12, 535.86.10")
	if !ok {
		t.Fatal("expected a 6-field row (no memory.free) to still parse")
	}
	if gpu.Name != "RTX 3090" || gpu.Memory != "24576 MB" {
		t.Errorf("core fields not parsed: %+v", gpu)
	}
	if gpu.MemoryFree != "" {
		t.Errorf("MemoryFree = %q, want empty (absent, not fabricated) when the field is missing", gpu.MemoryFree)
	}
}

func TestParseNvidiaSMICSVLine_TooFewFields(t *testing.T) {
	// Genuinely malformed rows (below the core field count) are rejected.
	if _, ok := parseNvidiaSMICSVLine("RTX 3090, 24576, 17827"); ok {
		t.Error("expected a row below the core field count to be rejected")
	}
	if _, ok := parseNvidiaSMICSVLine(""); ok {
		t.Error("expected an empty row to be rejected")
	}
}

func TestParseNvidiaSMICSVOutput_MultiGPUSkipsMalformed(t *testing.T) {
	out := "GPU0, 24576, 17827, 65, 12, 535.86.10, 6292\n" +
		"malformed row\n" +
		"GPU1, 24576, 1024, 40, 3, 535.86.10, 23000\n"
	gpus := parseNvidiaSMICSVOutput(out)
	if len(gpus) != 2 {
		t.Fatalf("expected 2 valid GPUs (malformed row skipped), got %d", len(gpus))
	}
	if gpus[0].Name != "GPU0" || gpus[1].Name != "GPU1" {
		t.Errorf("unexpected names: %q, %q", gpus[0].Name, gpus[1].Name)
	}
	if gpus[1].MemoryFree != "23000 MB" {
		t.Errorf("GPU1 MemoryFree = %q, want '23000 MB'", gpus[1].MemoryFree)
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
