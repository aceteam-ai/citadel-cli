package services

import "fmt"

// CUDAStartRefusal skips only a positively GPU-less Linux host. Unknown,
// including a failed driver probe on NVIDIA hardware, keeps the normal attempt.
func CUDAStartRefusal(name, goos string, gpuCount func() int) string {
	if goos != "linux" || !linuxOnlyServices[name] || gpuCount == nil || gpuCount() != 0 {
		return ""
	}
	return fmt.Sprintf("%s requires an NVIDIA GPU, not starting", name)
}

// AvailableServicesForHost applies live hardware policy to the OS build list.
// GetAvailableServices remains the static build inventory used by CLI help;
// heartbeats and interactive deployment choices use this host-specific list.
func AvailableServicesForHost(goos string, gpuCount func() int) []string {
	keys := availableServicesFor(goos)
	if goos != "linux" || gpuCount == nil || gpuCount() != 0 {
		return keys
	}
	filtered := make([]string, 0, len(keys))
	for _, name := range keys {
		if !linuxOnlyServices[name] {
			filtered = append(filtered, name)
		}
	}
	return filtered
}
