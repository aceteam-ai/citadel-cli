package services

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

var cudaOnlyFixture = []string{"bonsai", "unlimited-ocr", "vllm", "sglang", "diffusers", "omnivoice"}

func TestCUDAAvailabilitySkipsOnlyProvenAbsence(t *testing.T) {
	for _, count := range []int{0, 1, -1} {
		available := AvailableServicesForHost("linux", func() int { return count })
		set := map[string]bool{}
		for _, name := range available {
			set[name] = true
		}
		for _, name := range cudaOnlyFixture {
			refusal := CUDAStartRefusal(name, "linux", func() int { return count })
			if set[name] != (count != 0) || (refusal != "") != (count == 0) {
				t.Fatal("wrong CUDA availability", name, count, available, refusal)
			}
			if count == 0 && refusal != name+" requires an NVIDIA GPU, not starting" {
				t.Fatal("missing clear refusal", refusal)
			}
		}
		for _, name := range []string{"ollama", "llamacpp", "tei", "kokoro", "transcribe", "extraction"} {
			if !set[name] {
				t.Fatal("CPU engine withheld", name)
			}
			if CUDAStartRefusal(name, "linux", func() int { t.Fatal("CPU engine probed GPU"); return 0 }) != "" {
				t.Fatal("CPU engine refused")
			}
		}
		if strings.Join(available, ",") != strings.Join(availableServicesFor("linux"), ",") && count != 0 {
			t.Fatal("unknown/present GPU changed build inventory")
		}
	}
}

func TestCUDAAvailabilityOtherOSUnknownAndSampleCount(t *testing.T) {
	for _, goos := range []string{"darwin", "windows"} {
		probe := func() int { t.Fatal("other OS probed NVIDIA"); return 0 }
		if !reflect.DeepEqual(AvailableServicesForHost(goos, probe), availableServicesFor(goos)) {
			t.Fatal("OS availability changed", goos)
		}
		for _, name := range cudaOnlyFixture {
			if CUDAStartRefusal(name, goos, probe) != "" {
				t.Fatal("non-Linux start policy changed")
			}
		}
	}
	if !reflect.DeepEqual(AvailableServicesForHost("linux", nil), availableServicesFor("linux")) {
		t.Fatal("missing evidence suppressed a service")
	}
	for _, name := range cudaOnlyFixture {
		if CUDAStartRefusal(name, "linux", nil) != "" {
			t.Fatal("missing probe was absence")
		}
	}
	calls := 0
	AvailableServicesForHost("linux", func() int { calls++; return 0 })
	if calls != 1 {
		t.Fatal("advertisement sampled more than once", calls)
	}
	if CUDAStartRefusal("custom", "linux", func() int { t.Fatal("custom service probed GPU"); return 0 }) != "" {
		t.Fatal("custom engine refused")
	}
}

func TestCUDAAvailabilityPreservesOnlyServingAdoptableEngine(t *testing.T) {
	got := AvailableServicesForHost("linux", func() int { return 0 }, cudaOnlyFixture...)
	for _, name := range cudaOnlyFixture {
		if slices.Contains(got, name) != (name == "vllm") {
			t.Fatal("wrong external adoption availability", name, got)
		}
	}
	if !slices.IsSorted(got) {
		t.Fatal("availability order changed", got)
	}
}
