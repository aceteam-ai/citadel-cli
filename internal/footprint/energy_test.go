package footprint

import (
	"math"
	"testing"
	"time"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestWattsFromUtilTDP(t *testing.T) {
	cases := []struct {
		name string
		util float64
		tdp  float64
		want float64
	}{
		{"half load", 50, 350, 175},
		{"full load", 100, 450, 450},
		{"idle", 0, 350, 0},
		{"zero tdp yields zero", 80, 0, 0},
		{"negative tdp yields zero", 80, -10, 0},
		{"util clamped above 100", 150, 300, 300},
		{"util clamped below 0", -20, 300, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := wattsFromUtilTDP(c.util, c.tdp); !approx(got, c.want) {
				t.Errorf("wattsFromUtilTDP(%v, %v) = %v, want %v", c.util, c.tdp, got, c.want)
			}
		})
	}
}

func TestEnergyWh(t *testing.T) {
	cases := []struct {
		name     string
		powerW   float64
		interval time.Duration
		want     float64
	}{
		{"60s at 100W", 100, 60 * time.Second, 100.0 / 60.0},
		{"one hour at 200W", 200, time.Hour, 200},
		{"zero power", 0, time.Hour, 0},
		{"negative power", -5, time.Hour, 0},
		{"zero interval", 100, 0, 0},
		{"negative interval", 100, -time.Second, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := energyWh(c.powerW, c.interval); !approx(got, c.want) {
				t.Errorf("energyWh(%v, %v) = %v, want %v", c.powerW, c.interval, got, c.want)
			}
		})
	}
}

func TestEstimateNodePowerWaterfall(t *testing.T) {
	cases := []struct {
		name       string
		in         PowerInputs
		wantKnown  bool
		wantWatts  float64
		wantSource PowerSource
	}{
		{
			name: "tier1 measured GPU wins even when util+tdp present",
			in: PowerInputs{
				HasGPU: true, GPUPowerMeasured: true, GPUPowerWatts: 142.5,
				GPUUtilKnown: true, GPUUtilPercent: 90, GPUTDPWatts: 350,
				CPUKnown: true, CPUPercent: 50, CPUTDPWatts: 65,
			},
			wantKnown: true, wantWatts: 142.5, wantSource: PowerSourceMeasured,
		},
		{
			name: "tier2 GPU util times TDP when no measured draw",
			in: PowerInputs{
				HasGPU: true, GPUPowerMeasured: false,
				GPUUtilKnown: true, GPUUtilPercent: 40, GPUTDPWatts: 350,
				CPUKnown: true, CPUPercent: 100, CPUTDPWatts: 65,
			},
			wantKnown: true, wantWatts: 140, wantSource: PowerSourceEstimated,
		},
		{
			name: "tier2 skipped when TDP unknown, falls to CPU",
			in: PowerInputs{
				HasGPU: true, GPUPowerMeasured: false,
				GPUUtilKnown: true, GPUUtilPercent: 40, GPUTDPWatts: 0,
				CPUKnown: true, CPUPercent: 50, CPUTDPWatts: 65,
			},
			wantKnown: true, wantWatts: 32.5, wantSource: PowerSourceEstimated,
		},
		{
			name: "tier3 CPU floor (Apple Silicon / CPU-only node, no GPU)",
			in: PowerInputs{
				HasGPU:   false,
				CPUKnown: true, CPUPercent: 25, CPUTDPWatts: 60,
			},
			wantKnown: true, wantWatts: 15, wantSource: PowerSourceEstimated,
		},
		{
			name: "tier4 nothing usable yields unknown",
			in: PowerInputs{
				HasGPU:   false,
				CPUKnown: true, CPUPercent: 50, CPUTDPWatts: 0,
			},
			wantKnown: false,
		},
		{
			name: "no GPU util reading and no CPU TDP yields unknown",
			in: PowerInputs{
				HasGPU: true, GPUPowerMeasured: false, GPUUtilKnown: false,
				GPUTDPWatts: 350, CPUKnown: false, CPUTDPWatts: 65,
			},
			wantKnown: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EstimateNodePower(c.in)
			if got.Known != c.wantKnown {
				t.Fatalf("Known = %v, want %v", got.Known, c.wantKnown)
			}
			if !c.wantKnown {
				return
			}
			if !approx(got.Watts, c.wantWatts) {
				t.Errorf("Watts = %v, want %v", got.Watts, c.wantWatts)
			}
			if got.Source != c.wantSource {
				t.Errorf("Source = %q, want %q", got.Source, c.wantSource)
			}
		})
	}
}

func TestPowerConfigFromEnv(t *testing.T) {
	t.Run("defaults when unset", func(t *testing.T) {
		t.Setenv("CITADEL_GPU_TDP_WATTS", "")
		t.Setenv("CITADEL_CPU_TDP_WATTS", "")
		cfg := PowerConfigFromEnv()
		if cfg.GPUTDPWattsOverride != 0 {
			t.Errorf("GPU override = %v, want 0", cfg.GPUTDPWattsOverride)
		}
		if !approx(cfg.CPUTDPWatts, DefaultCPUTDPWatts) {
			t.Errorf("CPU TDP = %v, want %v", cfg.CPUTDPWatts, DefaultCPUTDPWatts)
		}
	})
	t.Run("honors valid overrides", func(t *testing.T) {
		t.Setenv("CITADEL_GPU_TDP_WATTS", "450")
		t.Setenv("CITADEL_CPU_TDP_WATTS", "125")
		cfg := PowerConfigFromEnv()
		if !approx(cfg.GPUTDPWattsOverride, 450) || !approx(cfg.CPUTDPWatts, 125) {
			t.Errorf("got %+v, want 450/125", cfg)
		}
	})
	t.Run("invalid CPU value falls back to default", func(t *testing.T) {
		t.Setenv("CITADEL_CPU_TDP_WATTS", "not-a-number")
		cfg := PowerConfigFromEnv()
		if !approx(cfg.CPUTDPWatts, DefaultCPUTDPWatts) {
			t.Errorf("CPU TDP = %v, want default %v", cfg.CPUTDPWatts, DefaultCPUTDPWatts)
		}
	})
	t.Run("negative value rejected", func(t *testing.T) {
		t.Setenv("CITADEL_GPU_TDP_WATTS", "-100")
		cfg := PowerConfigFromEnv()
		if cfg.GPUTDPWattsOverride != 0 {
			t.Errorf("negative GPU override should fall back to 0, got %v", cfg.GPUTDPWattsOverride)
		}
	})
}

func TestParseWattsField(t *testing.T) {
	cases := []struct {
		in     string
		wantOK bool
		want   float64
	}{
		{"142.35", true, 142.35},
		{" 90.0 ", true, 90.0},
		{"[N/A]", false, 0},
		{"[Not Supported]", false, 0},
		{"[Insufficient Permissions]", false, 0},
		{"", false, 0},
		{"-1", false, 0},
	}
	for _, c := range cases {
		got, ok := parseWattsField(c.in)
		if ok != c.wantOK || (ok && !approx(got, c.want)) {
			t.Errorf("parseWattsField(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}
