package platform

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v3/mem"
)

// GPUInfo represents information about a detected GPU
type GPUInfo struct {
	Name        string
	Memory      string
	MemoryUsed  string
	Temperature string
	Utilization string
	Driver      string
	// MemoryFree is free VRAM as reported directly by nvidia-smi's
	// memory.free query (citadel #833). It is NOT total-minus-used: nvidia-smi
	// reserves some memory (driver/ECC overhead) that counts against neither
	// total-as-free nor used, so total-used systematically overstates what is
	// actually available — the wrong direction of error for a value that
	// exists to prevent an OOM placement. Empty when unknown (e.g. the Metal
	// detector on macOS, which does not query it).
	MemoryFree string
	// Cores is the GPU core count reported by the macOS Metal detector
	// (system_profiler's "Total Number of Cores:"). Apple Silicon reports a GPU
	// core count instead of a discrete VRAM figure; it is kept here as a
	// structured integer rather than being formatted into Memory as text
	// (citadel-cli#1042 — the old code wrote "N cores" into Memory, which then
	// failed the numeric MemoryTotalMB parse downstream). Zero when unknown.
	Cores int
	// Unified is true for an Apple Silicon integrated GPU that shares one
	// unified memory pool with the CPU (no discrete VRAM). Consumers use it to
	// report a unified-memory budget instead of dedicated VRAM and to avoid
	// treating the node as a discrete-GPU node for CUDA queue routing.
	Unified bool
}

// GPUDetector interface defines operations for GPU detection
type GPUDetector interface {
	HasGPU() bool
	GetGPUInfo() ([]GPUInfo, error)
	GetGPUCount() int
}

// nvidiaSMIQueryGPUFields is the shared --query-gpu field list for the Linux
// and Windows detectors. memory.free is appended LAST so existing field
// indices (0-5) never shift; parseNvidiaSMICSVLine reads it at index 6.
const nvidiaSMIQueryGPUFields = "--query-gpu=name,memory.total,memory.used,temperature.gpu,utilization.gpu,driver_version,memory.free"

// nvidiaSMIQueryGPUFieldCount is the number of CSV fields nvidiaSMIQueryGPUFields
// produces per row; a row with fewer fields is skipped as malformed.
const nvidiaSMIQueryGPUFieldCount = 7

// parseNvidiaSMICSVOutput parses the full CSV output of
// `nvidia-smi <nvidiaSMIQueryGPUFields> --format=csv,noheader,nounits` into
// GPUInfo values, skipping any malformed row. Shared by the Linux and Windows
// detectors so the field layout lives in exactly one place.
func parseNvidiaSMICSVOutput(output string) []GPUInfo {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	gpus := make([]GPUInfo, 0, len(lines))
	for _, line := range lines {
		if gpu, ok := parseNvidiaSMICSVLine(line); ok {
			gpus = append(gpus, gpu)
		}
	}
	return gpus
}

// nvidiaSMICoreFieldCount is the pre-#833 field count (name through
// driver_version). A row with fewer than this is genuinely malformed and
// skipped. memory.free (index 6) is read when present but its absence does
// NOT reject the row: if some driver/GPU combination ever omits or rejects
// memory.free on a subset of rows, GPU detection as a whole (name/memory/
// temp/util/driver — which capability detection and inference-queue routing
// depend on) must keep working. MemoryFree simply stays empty in that case,
// same as the darwin/Metal detector, which never populates it at all.
const nvidiaSMICoreFieldCount = 6

// parseNvidiaSMICSVLine parses one CSV row produced by nvidiaSMIQueryGPUFields
// (name, memory.total, memory.used, temperature.gpu, utilization.gpu,
// driver_version, memory.free — all with "csv,noheader,nounits" formatting, so
// units are appended back on here). Pure and unit-tested directly against CSV
// fixtures, independent of a real nvidia-smi binary.
func parseNvidiaSMICSVLine(line string) (GPUInfo, bool) {
	parts := strings.Split(line, ",")
	if len(parts) < nvidiaSMICoreFieldCount {
		return GPUInfo{}, false
	}
	gpu := GPUInfo{
		Name:        strings.TrimSpace(parts[0]),
		Memory:      strings.TrimSpace(parts[1]) + " MB",
		MemoryUsed:  strings.TrimSpace(parts[2]) + " MB",
		Temperature: strings.TrimSpace(parts[3]) + "°C",
		Utilization: strings.TrimSpace(parts[4]) + "%",
		Driver:      strings.TrimSpace(parts[5]),
	}
	if len(parts) >= nvidiaSMIQueryGPUFieldCount {
		gpu.MemoryFree = strings.TrimSpace(parts[6]) + " MB"
	}
	return gpu, true
}

// NvidiaSMIExitHint translates a known nvidia-smi exit code into a
// user-friendly message with remediation guidance. Returns an empty string
// for unrecognized codes.
func NvidiaSMIExitHint(code int) string {
	switch code {
	case 6:
		return "No NVIDIA GPU detected by the driver"
	case 9:
		return "GPU hardware error — may need reseating or a power cycle"
	case 15:
		return "Driver version mismatch. Reboot required after driver update"
	case 18:
		return "NVIDIA drivers not loaded. Install drivers with: sudo apt install nvidia-driver-<version>"
	default:
		return ""
	}
}

// NvidiaSMIErrorMessage returns a human-readable message for an nvidia-smi
// execution error. It distinguishes between "binary not found" and known
// exit codes, falling back to the raw error for anything else.
func NvidiaSMIErrorMessage(err error) string {
	if err == nil {
		return ""
	}

	// Binary not found on PATH
	var pathErr *exec.Error
	if errors.As(err, &pathErr) {
		return "nvidia-smi not found — NVIDIA drivers are not installed"
	}

	// Binary ran but returned a non-zero exit code.
	// Check stderr first — it is more specific than the exit code alone.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
			if strings.Contains(stderr, "Driver/library version mismatch") {
				return "Driver/library version mismatch — reboot required after driver update"
			}
			if strings.Contains(stderr, "NVML Shared Library Not Found") {
				return "nvidia-smi not found — NVIDIA drivers are not installed"
			}
			// Unknown stderr content — surface it directly
			return fmt.Sprintf("nvidia-smi: %s", stderr)
		}

		// No stderr captured — fall back to exit-code heuristic
		code := exitErr.ExitCode()
		if hint := NvidiaSMIExitHint(code); hint != "" {
			return hint
		}
		return fmt.Sprintf("nvidia-smi exited with code %d", code)
	}

	return fmt.Sprintf("nvidia-smi error: %v", err)
}

// DetectNvidiaHardware checks via lspci whether any NVIDIA GPU hardware is
// physically present, regardless of driver status. Returns the GPU name
// (e.g. "NVIDIA Corporation GA102 [GeForce RTX 3090]") or an empty string.
// This covers both VGA and 3D controller entries (headless/datacenter GPUs
// like A100/H100 enumerate as "3D controller").
func DetectNvidiaHardware() string {
	cmd := exec.Command("sh", "-c", "lspci 2>/dev/null | grep -iE '(VGA compatible controller|3D controller).*NVIDIA'")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(output))
	// lspci output example: "01:00.0 VGA compatible controller: NVIDIA Corporation GA104 [GeForce RTX 3060 Ti] (rev a1)"
	// Extract the part after the controller type.
	for _, prefix := range []string{"VGA compatible controller: ", "3D controller: "} {
		if idx := strings.Index(line, prefix); idx >= 0 {
			return strings.TrimSpace(line[idx+len(prefix):])
		}
	}
	if line != "" {
		return line
	}
	return ""
}

// GetGPUDetector returns the appropriate GPU detector for the current OS
func GetGPUDetector() (GPUDetector, error) {
	switch runtime.GOOS {
	case "linux":
		return &LinuxGPUDetector{}, nil
	case "darwin":
		return &DarwinGPUDetector{}, nil
	case "windows":
		return &WindowsGPUDetector{}, nil
	default:
		return nil, fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}
}

// LinuxGPUDetector implements GPUDetector for Linux systems (NVIDIA)
type LinuxGPUDetector struct{}

func (l *LinuxGPUDetector) HasGPU() bool {
	// Check using lspci (matches both VGA and 3D controllers for headless/datacenter GPUs)
	cmd := exec.Command("sh", "-c", "lspci 2>/dev/null | grep -iE '(VGA compatible controller|3D controller).*NVIDIA'")
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
		nvidiaSMIQueryGPUFields,
		"--format=csv,noheader,nounits",
	)

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to query NVIDIA GPUs: %w", err)
	}

	return parseNvidiaSMICSVOutput(string(output)), nil
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

	gpus := parseDarwinGPUInfo(string(output))

	// Apple Silicon reports no discrete VRAM line, so parseDarwinGPUInfo leaves
	// Memory empty for a unified GPU. Enrich it here (live-host only, kept out
	// of the pure parser) with the total unified-memory pool so `citadel status`
	// shows a real memory figure instead of nothing. This is the TRUE pool (the
	// display=heartbeat=full-pool half of the three-quantity contract on
	// unifiedMemoryBudgetFraction; the vram:<n>gb routing tag uses the 0.75x
	// budget instead).
	if total := TotalRAMBytes(); total > 0 {
		for i := range gpus {
			if gpus[i].Unified && gpus[i].Memory == "" {
				gpus[i].Memory = fmt.Sprintf("%d MB", total/(1024*1024))
			}
		}
	}

	if len(gpus) == 0 {
		return nil, fmt.Errorf("no GPUs detected")
	}

	return gpus, nil
}

// parseDarwinGPUInfo parses the output of `system_profiler SPDisplaysDataType`
// into GPUInfo values. It is pure (no exec, no host state) so the parsing is
// unit-testable off a captured fixture. It keys only on the four line prefixes
// the macOS tool emits that this codebase already relies on: "Chipset Model:",
// "Total Number of Cores:", "VRAM (Total):", and "Metal:".
//
// Apple Silicon reports a GPU core count and NO discrete VRAM line, so the core
// count is stored in GPUInfo.Cores (not formatted into Memory — citadel-cli#1042)
// and Unified is set. An Intel Mac's discrete GPU reports a real "VRAM (Total):"
// line, which populates Memory with Unified left false.
func parseDarwinGPUInfo(output string) []GPUInfo {
	lines := strings.Split(output, "\n")
	gpus := []GPUInfo{}
	var currentGPU *GPUInfo

	for _, line := range lines {
		line = strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(line, "Chipset Model:"):
			if currentGPU != nil {
				gpus = append(gpus, *currentGPU)
			}
			name := strings.TrimSpace(strings.TrimPrefix(line, "Chipset Model:"))
			currentGPU = &GPUInfo{
				Name:    name,
				Unified: isAppleSiliconChipset(name),
			}
		case currentGPU == nil:
			// Ignore any lines before the first "Chipset Model:".
		case strings.HasPrefix(line, "VRAM (Total):"):
			if v := strings.TrimSpace(strings.TrimPrefix(line, "VRAM (Total):")); v != "" {
				currentGPU.Memory = v
			}
		case strings.HasPrefix(line, "Total Number of Cores:"):
			if v := strings.TrimSpace(strings.TrimPrefix(line, "Total Number of Cores:")); v != "" {
				if fields := strings.Fields(v); len(fields) > 0 {
					if n, err := strconv.Atoi(fields[0]); err == nil {
						currentGPU.Cores = n
					}
				}
			}
		case strings.HasPrefix(line, "Metal:"):
			currentGPU.Driver = "Metal " + strings.TrimSpace(strings.TrimPrefix(line, "Metal:"))
		}
	}

	if currentGPU != nil {
		gpus = append(gpus, *currentGPU)
	}

	return gpus
}

// isAppleSiliconChipset reports whether a system_profiler "Chipset Model" names
// an Apple Silicon integrated GPU (e.g. "Apple M1", "Apple M2 Max", "Apple M3
// Pro"). An Intel Mac reports an Intel/AMD chipset here instead, so this stays
// false for a discrete GPU with a real VRAM line.
func isAppleSiliconChipset(name string) bool {
	return strings.HasPrefix(strings.TrimSpace(name), "Apple M")
}

// AppleGPUFamily normalizes an Apple Silicon chipset model into a lowercase,
// tag-safe family identifier used for the gpu:apple-<family> capability tag:
// "Apple M2 Max" -> "m2-max", "Apple M1" -> "m1". Returns "" for a chipset that
// is not Apple Silicon (e.g. an Intel Mac's discrete GPU), so callers never
// mint an apple tag for non-Apple-Silicon hardware.
func AppleGPUFamily(chipset string) string {
	chipset = strings.TrimSpace(chipset)
	if !isAppleSiliconChipset(chipset) {
		return ""
	}
	fam := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(chipset, "Apple")))
	return strings.Join(strings.Fields(fam), "-")
}

// unifiedMemoryBudgetFraction is the conservative fraction of total unified
// memory advertised as the usable GPU budget for the vram:<n>gb routing tag on
// Apple Silicon. macOS lets Metal use most of unified memory but reserves a
// portion for the OS (Metal's recommendedMaxWorkingSetSize is roughly this on
// current Apple Silicon). This is a provisioning ESTIMATE, not a measured value
// (citadel-cli#1042) — it should be calibrated against a real Mac's
// recommendedMaxWorkingSetSize. Pinned by TestUnifiedMemoryBudgetMB.
//
// Three DELIBERATELY-DIFFERENT unified-memory quantities exist for a Mac; this
// is the single place that states the contract so `citadel status` (24 GB) vs
// `citadel capabilities` (18 GB) does not read as a bug:
//   - `citadel status` display (GPUInfo.Memory, enriched in getGPUInfoText) and
//     the heartbeat (status.GPUMetrics.MemoryTotalMB) both report the FULL pool
//     (total RAM) — the honest true size of the shared memory.
//   - the vram:<n>gb ROUTING tag reports this 0.75x budget — the conservative
//     usable capacity a scheduler should place against.
const unifiedMemoryBudgetFraction = 0.75

// UnifiedMemoryBudgetMB returns a conservative usable GPU memory budget in MB
// for an Apple Silicon node with totalRAMBytes of unified memory. Returns 0 for
// a zero/unknown input (no fabricated value).
func UnifiedMemoryBudgetMB(totalRAMBytes uint64) int {
	if totalRAMBytes == 0 {
		return 0
	}
	totalMB := float64(totalRAMBytes) / (1024 * 1024)
	return int(totalMB * unifiedMemoryBudgetFraction)
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

// TotalRAMBytes returns total physical RAM in bytes, or 0 when unavailable. It
// uses gopsutil's mem.VirtualMemory().Total — the same source internal/status
// uses for MemoryTotalGB — so the unified-memory figures reported by the
// capabilities layer and the heartbeat agree.
func TotalRAMBytes() uint64 {
	if v, err := mem.VirtualMemory(); err == nil {
		return v.Total
	}
	return 0
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
		nvidiaSMIQueryGPUFields,
		"--format=csv,noheader,nounits",
	)

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to query NVIDIA GPUs: %w", err)
	}

	return parseNvidiaSMICSVOutput(string(output)), nil
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
