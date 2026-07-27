package footprint

import (
	"os"
	"strconv"
	"time"
)

// PowerSource labels how a sample's power figure was obtained, so an auditor can
// tell a real hardware measurement apart from a modeled estimate. It is written
// verbatim into the power_source CSV column.
type PowerSource string

const (
	// PowerSourceMeasured means the figure came from a hardware power sensor
	// (nvidia-smi power.draw). This is the number the sovereignty receipt wants.
	PowerSourceMeasured PowerSource = "measured"
	// PowerSourceEstimated means the figure was modeled from utilisation and a
	// thermal design / power-limit budget, not read from a sensor.
	PowerSourceEstimated PowerSource = "estimated"
	// PowerSourceUnknown (empty) means no defensible figure was available; the CSV
	// leaves power_w / energy_wh / power_source blank rather than guessing.
	PowerSourceUnknown PowerSource = ""
)

// DefaultCPUTDPWatts is the fallback CPU package power budget used for the coarse
// CPU-utilisation floor when CITADEL_CPU_TDP_WATTS is unset. 65W is a typical
// desktop x86 TDP; it over-estimates Apple Silicon (M-series package power runs
// lower) and under-estimates a HEDT/server socket, so operators should set the
// env var to their chip's rated TDP for a tighter figure. It is only ever used as
// the last waterfall tier (see EstimateNodePower), so it never dilutes a node
// that reports real GPU power.
const DefaultCPUTDPWatts = 65.0

// PowerConfig holds the resolved, per-node power-estimation knobs. It is resolved
// once (from the environment) and reused every tick, so no env parsing happens on
// the sampling hot path.
type PowerConfig struct {
	// GPUTDPWattsOverride, when > 0, forces the TDP used for the GPU
	// utilisation-times-TDP estimate (waterfall tier 2), overriding the card's
	// reported power.limit. From CITADEL_GPU_TDP_WATTS. Zero means "use the
	// measured power.limit".
	GPUTDPWattsOverride float64
	// CPUTDPWatts is the CPU package power budget for the coarse CPU floor
	// (waterfall tier 3). From CITADEL_CPU_TDP_WATTS, defaulting to
	// DefaultCPUTDPWatts. A value <= 0 disables the CPU floor entirely.
	CPUTDPWatts float64
}

// PowerConfigFromEnv resolves the power-estimation knobs from the environment.
// Unset or unparseable values fall back to safe defaults; nothing here shells
// out or requires privileges.
func PowerConfigFromEnv() PowerConfig {
	return PowerConfig{
		GPUTDPWattsOverride: envFloat("CITADEL_GPU_TDP_WATTS", 0),
		CPUTDPWatts:         envFloat("CITADEL_CPU_TDP_WATTS", DefaultCPUTDPWatts),
	}
}

// envFloat reads a non-negative float from env, returning def when unset or
// invalid. A negative value is treated as invalid (falls back to def).
func envFloat(key string, def float64) float64 {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 {
		return def
	}
	return v
}

// PowerInputs is the set of signals feeding one node-level power decision. The
// *Known flags distinguish a real zero reading (idle GPU, 0% CPU) from "no
// signal", which the waterfall must not treat as a measurement.
type PowerInputs struct {
	// HasGPU reports whether a GPU is present at all.
	HasGPU bool
	// GPUPowerWatts is the summed measured board power (nvidia-smi power.draw).
	// Only meaningful when GPUPowerMeasured is true.
	GPUPowerWatts float64
	// GPUPowerMeasured is true when at least one GPU reported power.draw.
	GPUPowerMeasured bool
	// GPUUtilKnown / GPUUtilPercent carry the aggregate GPU utilisation (0-100).
	GPUUtilKnown   bool
	GPUUtilPercent float64
	// GPUTDPWatts is the TDP used for the util-based estimate (power.limit or the
	// configured override). Zero means "unknown", which skips tier 2.
	GPUTDPWatts float64
	// CPUKnown / CPUPercent carry the host CPU utilisation (0-100).
	CPUKnown   bool
	CPUPercent float64
	// CPUTDPWatts is the CPU package power budget for the coarse floor. Zero
	// disables tier 3.
	CPUTDPWatts float64
}

// PowerEstimate is the outcome of one node-level power decision.
type PowerEstimate struct {
	// Watts is the estimated instantaneous node power. Only meaningful when Known.
	Watts float64
	// Source labels how Watts was obtained (measured vs estimated).
	Source PowerSource
	// Known is false when no defensible figure was available at all.
	Known bool
}

// EstimateNodePower selects the best available node power figure via a strict
// fall-through waterfall. Each tier is tried only if it can produce a value; the
// first that can, wins. Tiers are NEVER combined, so a "measured" label always
// means a real sensor reading and is never diluted by a modeled term.
//
//  1. Measured GPU board power (nvidia-smi power.draw)  -> measured
//  2. GPU utilisation x TDP (power.limit or override)   -> estimated
//  3. CPU utilisation x CPU TDP (coarse floor)          -> estimated
//  4. Nothing usable                                    -> unknown (blank)
//
// Tier 3 is what gives Apple Silicon and CPU-only nodes a conservative floor
// without powermetrics (which needs sudo): they have no power.draw, no NVIDIA
// power.limit, and no GPU util, so they fall cleanly to the CPU model.
//
// A fuller node model (GPU + CPU + PSU efficiency + idle baseline summed) is a
// deliberate next increment; this first cut reports a single, clearly-labeled
// dominant term.
func EstimateNodePower(in PowerInputs) PowerEstimate {
	if in.HasGPU && in.GPUPowerMeasured {
		return PowerEstimate{Watts: in.GPUPowerWatts, Source: PowerSourceMeasured, Known: true}
	}
	if in.HasGPU && in.GPUUtilKnown && in.GPUTDPWatts > 0 {
		return PowerEstimate{
			Watts:  wattsFromUtilTDP(in.GPUUtilPercent, in.GPUTDPWatts),
			Source: PowerSourceEstimated,
			Known:  true,
		}
	}
	if in.CPUKnown && in.CPUTDPWatts > 0 {
		return PowerEstimate{
			Watts:  wattsFromUtilTDP(in.CPUPercent, in.CPUTDPWatts),
			Source: PowerSourceEstimated,
			Known:  true,
		}
	}
	return PowerEstimate{}
}

// wattsFromUtilTDP models power as a linear fraction of a TDP budget: a device at
// util% of capacity draws util% of its TDP. This is a coarse first-order model
// (real curves have an idle floor and are convex), but it is defensible, needs no
// sensor, and is clearly labeled "estimated". The utilisation is clamped to
// 0..100 so a bogus reading cannot produce a negative or super-TDP figure. A
// non-positive TDP yields 0.
func wattsFromUtilTDP(utilPercent, tdpWatts float64) float64 {
	if tdpWatts <= 0 {
		return 0
	}
	frac := utilPercent / 100
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	return frac * tdpWatts
}

// energyWh converts an average power over an interval into watt-hours. This is a
// left-Riemann approximation: each sample is credited one full interval at its
// sampled power, so summing energy_wh across a day's node rows yields the
// auditable daily energy total. A non-positive power or interval yields 0.
func energyWh(powerW float64, interval time.Duration) float64 {
	if powerW <= 0 || interval <= 0 {
		return 0
	}
	return powerW * interval.Hours()
}
