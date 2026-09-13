package platform

import (
	"strings"
	"testing"
)

// appleSiliconSPFixture is a SYNTHETIC `system_profiler SPDisplaysDataType`
// sample for an Apple Silicon Mac. It is modeled on the exact four line
// prefixes parseDarwinGPUInfo keys on ("Chipset Model:", "Total Number of
// Cores:", "Metal:"); Apple Silicon reports NO "VRAM (Total):" line. Capturing
// a real sample from a physical Mac into testdata and confirming this parser is
// the first real-Mac step noted in the PR (citadel-cli#1042).
const appleSiliconSPFixture = `Graphics/Displays:

    Apple M2 Max:

      Chipset Model: Apple M2 Max
      Type: GPU
      Bus: Built-In
      Total Number of Cores: 38
      Vendor: Apple (0x106b)
      Metal: Supported, feature set macOS GPUFamily2 v1
`

// intelMacSPFixture is a SYNTHETIC sample for an Intel Mac with a discrete GPU,
// which DOES report a "VRAM (Total):" line and no unified core count.
const intelMacSPFixture = `Graphics/Displays:

    AMD Radeon Pro 5500M:

      Chipset Model: AMD Radeon Pro 5500M
      Type: GPU
      Bus: PCIe
      VRAM (Total): 8 GB
      Vendor: AMD (0x1002)
      Metal: Metal 3
`

func TestParseDarwinGPUInfo_AppleSilicon(t *testing.T) {
	gpus := parseDarwinGPUInfo(appleSiliconSPFixture)
	if len(gpus) != 1 {
		t.Fatalf("expected 1 GPU, got %d: %+v", len(gpus), gpus)
	}
	g := gpus[0]
	if g.Name != "Apple M2 Max" {
		t.Errorf("Name = %q, want %q", g.Name, "Apple M2 Max")
	}
	if !g.Unified {
		t.Error("Unified = false, want true for Apple Silicon")
	}
	if g.Cores != 38 {
		t.Errorf("Cores = %d, want 38", g.Cores)
	}
	// The exact system_profiler "Metal:" line format is version-dependent, so
	// assert only that it was parsed into the Driver (prefix), not an exact value.
	if !strings.HasPrefix(g.Driver, "Metal ") {
		t.Errorf("Driver = %q, want a value beginning with %q", g.Driver, "Metal ")
	}
	// The core count must NOT be formatted into Memory (the citadel-cli#1042
	// bug): Apple Silicon reports no VRAM line, so Memory stays empty here (the
	// live wiring enriches it with total RAM; the pure parser leaves it empty).
	if g.Memory != "" {
		t.Errorf("Memory = %q, want empty (cores must not be written into Memory)", g.Memory)
	}
}

func TestParseDarwinGPUInfo_IntelMacDiscreteGPU(t *testing.T) {
	gpus := parseDarwinGPUInfo(intelMacSPFixture)
	if len(gpus) != 1 {
		t.Fatalf("expected 1 GPU, got %d: %+v", len(gpus), gpus)
	}
	g := gpus[0]
	if g.Unified {
		t.Error("Unified = true, want false for an Intel Mac discrete GPU")
	}
	if g.Memory != "8 GB" {
		t.Errorf("Memory = %q, want %q", g.Memory, "8 GB")
	}
	if g.Cores != 0 {
		t.Errorf("Cores = %d, want 0 (discrete GPU reports no unified core count)", g.Cores)
	}
}

func TestParseDarwinGPUInfo_NeverWritesCoresIntoMemory(t *testing.T) {
	// Regression for citadel-cli#1042: the old parser wrote "N cores" into
	// Memory, which then failed the numeric MemoryTotalMB parse downstream.
	for _, fx := range []string{appleSiliconSPFixture, intelMacSPFixture} {
		for _, g := range parseDarwinGPUInfo(fx) {
			if got := g.Memory; got != "" && got != "8 GB" {
				t.Errorf("Memory = %q must never contain a cores string", got)
			}
		}
	}
}

func TestAppleGPUFamily(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Apple M2 Max", "m2-max"},
		{"Apple M1", "m1"},
		{"Apple M3 Pro", "m3-pro"},
		{"Apple M4 Max", "m4-max"},
		{"  Apple M2  ", "m2"},
		{"AMD Radeon Pro 5500M", ""},
		{"Intel Iris Plus Graphics", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := AppleGPUFamily(c.in); got != c.want {
			t.Errorf("AppleGPUFamily(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestUnifiedMemoryBudgetMB(t *testing.T) {
	const gb = uint64(1024 * 1024 * 1024)
	cases := []struct {
		name  string
		bytes uint64
		want  int
	}{
		{"zero is unknown", 0, 0},
		{"24GB at 0.75", 24 * gb, 18432}, // 24576 * 0.75
		{"16GB at 0.75", 16 * gb, 12288}, // 16384 * 0.75
		{"8GB at 0.75", 8 * gb, 6144},    // 8192 * 0.75
		{"64GB at 0.75", 64 * gb, 49152}, // 65536 * 0.75
	}
	for _, c := range cases {
		if got := UnifiedMemoryBudgetMB(c.bytes); got != c.want {
			t.Errorf("%s: UnifiedMemoryBudgetMB(%d) = %d, want %d", c.name, c.bytes, got, c.want)
		}
	}
}
