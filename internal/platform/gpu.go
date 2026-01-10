package platform

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// GPUInfo represents information about a detected GPU
type GPUInfo struct {
	Name        string
	Memory      string
	Temperature string
	Utilization string
	Driver      string
}

// GPUDetector interface defines operations for GPU detection
type GPUDetector interface {
	HasGPU() bool
	GetGPUInfo() ([]GPUInfo, error)
	GetGPUCount() int
}

// GetGPUDetector returns the appropriate GPU detector for the current OS
func GetGPUDetector() (GPUDetector, error) {
	switch OS() {
	case "linux":
		return &LinuxGPUDetector{}, nil
	case "darwin":
		return &DarwinGPUDetector{}, nil
	case "windows":
		return &WindowsGPUDetector{}, nil
	default:
		return nil, fmt.Errorf("unsupported operating system: %s", OS())
	}
}

// LinuxGPUDetector implements GPUDetector for Linux systems (NVIDIA)
type LinuxGPUDetector struct{}

func (l *LinuxGPUDetector) HasGPU() bool {
	// Check using lspci
	cmd := exec.Command("sh", "-c", "lspci | grep -i 'VGA compatible controller.*NVIDIA'")
	if err := cmd.Run(); err == nil {
		return true
	}

	// Check using nvidia-smi
	cmd = exec.Command("nvidia-smi")
	return cmd.Run() == nil
}

func (l *LinuxGPUDetector) GetGPUCount() int {
	cmd := exec.Command("nvidia-smi", "--query-gpu=name", "--format=csv,noheader")
	output, err := cmd.Output()
	if err != nil {
		return 0
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	return len(lines)
}

func (l *LinuxGPUDetector) GetGPUInfo() ([]GPUInfo, error) {
	cmd := exec.Command(
		"nvidia-smi",
		"--query-gpu=name,memory.total,temperature.gpu,utilization.gpu,driver_version",
		"--format=csv,noheader,nounits",
	)

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to query NVIDIA GPUs: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	gpus := make([]GPUInfo, 0, len(lines))

	for _, line := range lines {
		parts := strings.Split(line, ",")
		if len(parts) < 5 {
			continue
		}

		gpu := GPUInfo{
			Name:        strings.TrimSpace(parts[0]),
			Memory:      strings.TrimSpace(parts[1]) + " MB",
			Temperature: strings.TrimSpace(parts[2]) + "°C",
			Utilization: strings.TrimSpace(parts[3]) + "%",
			Driver:      strings.TrimSpace(parts[4]),
		}
		gpus = append(gpus, gpu)
	}

	return gpus, nil
}

// DarwinGPUDetector implements GPUDetector for macOS systems
type DarwinGPUDetector struct{}

func (d *DarwinGPUDetector) HasGPU() bool {
	// On macOS, check for Metal-compatible GPUs using system_profiler
	cmd := exec.Command("system_profiler", "SPDisplaysDataType")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	// Look for chipset or GPU information
	return strings.Contains(string(output), "Chipset Model:") ||
		strings.Contains(string(output), "Metal:")
}

func (d *DarwinGPUDetector) GetGPUCount() int {
	gpus, err := d.GetGPUInfo()
	if err != nil {
		return 0
	}
	return len(gpus)
}

func (d *DarwinGPUDetector) GetGPUInfo() ([]GPUInfo, error) {
	// Use text parsing for GPU info on macOS
	return d.getGPUInfoText()
}

func (d *DarwinGPUDetector) getGPUInfoText() ([]GPUInfo, error) {
	cmd := exec.Command("system_profiler", "SPDisplaysDataType")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to query GPU info: %w", err)
	}

	lines := strings.Split(string(output), "\n")
	gpus := []GPUInfo{}
	var currentGPU *GPUInfo

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "Chipset Model:") {
			if currentGPU != nil {
				gpus = append(gpus, *currentGPU)
			}
			currentGPU = &GPUInfo{
				Name: strings.TrimSpace(strings.TrimPrefix(line, "Chipset Model:")),
			}
		} else if currentGPU != nil {
			if strings.HasPrefix(line, "VRAM (Total):") || strings.HasPrefix(line, "Total Number of Cores:") {
				memStr := strings.TrimSpace(strings.TrimPrefix(line, "VRAM (Total):"))
				if memStr == "" {
					memStr = strings.TrimSpace(strings.TrimPrefix(line, "Total Number of Cores:"))
					if memStr != "" {
						currentGPU.Memory = memStr + " cores"
					}
				} else {
					currentGPU.Memory = memStr
				}
			} else if strings.HasPrefix(line, "Metal:") {
				currentGPU.Driver = "Metal " + strings.TrimSpace(strings.TrimPrefix(line, "Metal:"))
			}
		}
	}

	if currentGPU != nil {
		gpus = append(gpus, *currentGPU)
	}

	if len(gpus) == 0 {
		return nil, fmt.Errorf("no GPUs detected")
	}

	return gpus, nil
}

// FormatGPUInfo returns a human-readable string representation of GPU info
func FormatGPUInfo(gpus []GPUInfo) string {
	if len(gpus) == 0 {
		return "No GPU detected"
	}

	var sb strings.Builder
	for i, gpu := range gpus {
		sb.WriteString(fmt.Sprintf("GPU %d: %s\n", i, gpu.Name))
		if gpu.Memory != "" {
			sb.WriteString(fmt.Sprintf("  Memory: %s\n", gpu.Memory))
		}
		if gpu.Temperature != "" {
			sb.WriteString(fmt.Sprintf("  Temperature: %s\n", gpu.Temperature))
		}
		if gpu.Utilization != "" {
			sb.WriteString(fmt.Sprintf("  Utilization: %s\n", gpu.Utilization))
		}
		if gpu.Driver != "" {
			sb.WriteString(fmt.Sprintf("  Driver: %s\n", gpu.Driver))
		}
	}

	return sb.String()
}

// GetGPUCountSimple is a helper function that returns the number of GPUs or 0 if detection fails
func GetGPUCountSimple() int {
	detector, err := GetGPUDetector()
	if err != nil {
		return 0
	}
	return detector.GetGPUCount()
}

// GetGPUMemoryMB returns the total GPU memory in MB (best effort)
func GetGPUMemoryMB() int {
	detector, err := GetGPUDetector()
	if err != nil {
		return 0
	}

	gpus, err := detector.GetGPUInfo()
	if err != nil || len(gpus) == 0 {
		return 0
	}

	// Try to parse memory from first GPU
	memStr := gpus[0].Memory
	// Remove " MB" or " GB" suffix and parse
	memStr = strings.TrimSuffix(memStr, " MB")
	memStr = strings.TrimSuffix(memStr, "MB")

	if strings.Contains(memStr, "GB") {
		memStr = strings.TrimSuffix(memStr, " GB")
		memStr = strings.TrimSuffix(memStr, "GB")
		if gb, err := strconv.ParseFloat(strings.TrimSpace(memStr), 64); err == nil {
			return int(gb * 1024)
		}
	}

	if mb, err := strconv.Atoi(strings.TrimSpace(memStr)); err == nil {
		return mb
	}

	return 0
}

// WindowsGPUDetector implements GPUDetector for Windows systems (NVIDIA)
type WindowsGPUDetector struct{}

func (w *WindowsGPUDetector) HasGPU() bool {
	// Primary: Check for nvidia-smi.exe in standard location
	nvidiaSmiPath := `C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe`
	if _, err := os.Stat(nvidiaSmiPath); err == nil {
		cmd := exec.Command(nvidiaSmiPath)
		return cmd.Run() == nil
	}

	// Fallback: Check PATH for nvidia-smi
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		cmd := exec.Command("nvidia-smi")
		return cmd.Run() == nil
	}

	// Final fallback: Use WMI to detect NVIDIA GPU
	return w.hasGPUViaWMI()
}

func (w *WindowsGPUDetector) GetGPUCount() int {
	cmd := w.nvidiaSmiCommand("--query-gpu=name", "--format=csv,noheader")
	output, err := cmd.Output()
	if err != nil {
		return 0
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	return len(lines)
}

func (w *WindowsGPUDetector) GetGPUInfo() ([]GPUInfo, error) {
	cmd := w.nvidiaSmiCommand(
		"--query-gpu=name,memory.total,temperature.gpu,utilization.gpu,driver_version",
		"--format=csv,noheader,nounits",
	)

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to query NVIDIA GPUs: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	gpus := make([]GPUInfo, 0, len(lines))

	for _, line := range lines {
		parts := strings.Split(line, ",")
		if len(parts) < 5 {
			continue
		}

		gpu := GPUInfo{
			Name:        strings.TrimSpace(parts[0]),
			Memory:      strings.TrimSpace(parts[1]) + " MB",
			Temperature: strings.TrimSpace(parts[2]) + "°C",
			Utilization: strings.TrimSpace(parts[3]) + "%",
			Driver:      strings.TrimSpace(parts[4]),
		}
		gpus = append(gpus, gpu)
	}

	return gpus, nil
}

// nvidiaSmiCommand creates an exec.Cmd for nvidia-smi with the given arguments
// Tries standard Windows installation path first, then falls back to PATH
func (w *WindowsGPUDetector) nvidiaSmiCommand(args ...string) *exec.Cmd {
	// Try standard installation path first
	nvidiaSmiPath := `C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe`
	if _, err := os.Stat(nvidiaSmiPath); err == nil {
		return exec.Command(nvidiaSmiPath, args...)
	}

	// Fallback to PATH
	return exec.Command("nvidia-smi", args...)
}

// hasGPUViaWMI uses WMI to check for NVIDIA GPUs as a fallback method
func (w *WindowsGPUDetector) hasGPUViaWMI() bool {
	cmd := exec.Command("wmic", "path", "win32_VideoController", "get", "name")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(output)), "nvidia")
}

