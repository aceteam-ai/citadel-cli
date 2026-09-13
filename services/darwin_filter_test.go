package services

import "testing"

// TestAvailableServicesFor_DarwinFiltersCUDAEngines pins that darwin advertises
// only the darwin-capable engines and never a CUDA-only engine
// (citadel-cli#1042).
func TestAvailableServicesFor_DarwinFiltersCUDAEngines(t *testing.T) {
	darwin := availableServicesFor("darwin")
	inDarwin := make(map[string]bool, len(darwin))
	for _, s := range darwin {
		inDarwin[s] = true
	}

	mustInclude := []string{"ollama", "llamacpp", "lmstudio"}
	for _, s := range mustInclude {
		if !inDarwin[s] {
			t.Errorf("darwin available services missing %q; got %v", s, darwin)
		}
	}

	mustExclude := []string{"vllm", "sglang", "bonsai", "diffusers", "unlimited-ocr", "omnivoice"}
	for _, s := range mustExclude {
		if inDarwin[s] {
			t.Errorf("darwin available services must NOT advertise CUDA-only engine %q; got %v", s, darwin)
		}
	}
}

// TestAvailableServicesFor_NonDarwinIsFullMap pins that every non-darwin OS gets
// the complete ServiceMap (no filtering), so linux/windows behavior is unchanged.
func TestAvailableServicesFor_NonDarwinIsFullMap(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		got := availableServicesFor(goos)
		if len(got) != len(ServiceMap) {
			t.Errorf("availableServicesFor(%q) returned %d services, want all %d", goos, len(got), len(ServiceMap))
		}
	}
}

// TestServiceMapDarwinClassificationExhaustive forces every ServiceMap key to be
// classified as darwin-capable OR linux-only, exactly once. This mirrors the
// repo's TestServiceMapBindSweep / TestEngineCacheDirsMatchComposeMounts pattern:
// a newly added engine must make a classification decision at PR time rather
// than silently defaulting into (or out of) the darwin advertisement.
func TestServiceMapDarwinClassificationExhaustive(t *testing.T) {
	for name := range ServiceMap {
		capable := darwinCapableServices[name]
		linuxOnly := linuxOnlyServices[name]
		switch {
		case capable && linuxOnly:
			t.Errorf("engine %q is classified as BOTH darwin-capable and linux-only; pick one", name)
		case !capable && !linuxOnly:
			t.Errorf("engine %q is unclassified: add it to darwinCapableServices or linuxOnlyServices", name)
		}
	}
	// And the reverse: neither set may name an engine that is not in ServiceMap.
	for name := range darwinCapableServices {
		if _, ok := ServiceMap[name]; !ok {
			t.Errorf("darwinCapableServices names %q which is not in ServiceMap", name)
		}
	}
	for name := range linuxOnlyServices {
		if _, ok := ServiceMap[name]; !ok {
			t.Errorf("linuxOnlyServices names %q which is not in ServiceMap", name)
		}
	}
}
