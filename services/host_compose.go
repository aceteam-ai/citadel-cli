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

// HostComposeSelection records a template and whether existing bytes must stay.
// gpuCount is injected: zero proves absence, a positive count proves presence,
// and a negative count (or nil detector) is unknown.
type HostComposeSelection struct {
	Content          string
	Embedded         bool
	PreserveExisting bool
}

func SelectComposeForHost(name, goos string, gpuCount func() int) HostComposeSelection {
	content, ok := ServiceMap[name]
	selected := HostComposeSelection{Content: content, Embedded: ok}
	if !ok || goos != "linux" || !HasCPUComposeVariant(name) {
		return selected
	}
	count := -1
	if gpuCount != nil {
		count = gpuCount()
	}
	if count < 0 {
		selected.PreserveExisting = true
		return selected
	}
	if count > 0 {
		return selected
	}
	if name == "ollama" {
		selected.Content = ollamaCPUCompose
	} else {
		selected.Content = llamacppCPUCompose
	}
	return selected
}

// ComposeForHost provides the initial template. Callers reconciling existing
// files must honor SelectComposeForHost's PreserveExisting on unknown evidence.
func ComposeForHost(name, goos string, gpuCount func() int) (string, bool) {
	selected := SelectComposeForHost(name, goos, gpuCount)
	return selected.Content, selected.Embedded
}

// ComposeMapForHost returns a fresh map and samples the detector at most once.
func ComposeMapForHost(goos string, gpuCount func() int) map[string]string {
	result := make(map[string]string, len(ServiceMap))
	count, sampled := -1, false
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

// ComposeMapForRefresh withholds uncertain CPU/GPU selections from a sweep,
// so even a version upgrade cannot flip the existing flavor on probe failure.
func ComposeMapForRefresh(goos string, gpuCount func() int) map[string]string {
	count := -1
	if goos == "linux" && gpuCount != nil {
		count = gpuCount()
	}
	selected := ComposeMapForHost(goos, func() int { return count })
	if goos == "linux" && count < 0 {
		delete(selected, "ollama")
		delete(selected, "llamacpp")
	}
	return selected
}
