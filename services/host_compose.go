package services

import _ "embed"

// Linux hosts without NVIDIA use the same CPU templates as Darwin. Keep the
// static ServiceMap unchanged: callers choose at materialization time so a
// later start can observe a GPU being added or removed.
//
//go:embed compose/ollama.darwin.yml
var ollamaCPUCompose string

//go:embed compose/llamacpp.darwin.yml
var llamacppCPUCompose string

// HasCPUComposeVariant limits CPU selection to engines with a supported image.
func HasCPUComposeVariant(name string) bool { return name == "ollama" || name == "llamacpp" }

// ComposeForHost selects a template without probing unrelated engines or OSes.
// gpuCount is injected; production callers supply platform.GetGPUCountSimple.
func ComposeForHost(name, goos string, gpuCount func() int) (string, bool) {
	content, ok := ServiceMap[name]
	if !ok || goos != "linux" || !HasCPUComposeVariant(name) {
		return content, ok
	}
	if gpuCount != nil && gpuCount() > 0 {
		return content, true
	}
	if name == "ollama" {
		return ollamaCPUCompose, true
	}
	return llamacppCPUCompose, true
}

// ComposeMapForHost returns a fresh map and samples the detector at most once.
func ComposeMapForHost(goos string, gpuCount func() int) map[string]string {
	result := make(map[string]string, len(ServiceMap))
	count, sampled := 0, false
	once := func() int {
		if !sampled {
			if gpuCount != nil {
				count = gpuCount()
			}
			sampled = true
		}
		return count
	}
	for name := range ServiceMap {
		result[name], _ = ComposeForHost(name, goos, once)
	}
	return result
}

// ComposeDeclaresNvidiaReservation inspects only known embedded templates.
func ComposeDeclaresNvidiaReservation(content string) bool {
	return composeDeclaresNvidiaReservation(content)
}
