package cmd

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

// hotswap.go wires the model-hotswap swap manager (citadel-cli#632) to this
// node's real side-effects: a jobs.ServiceHandler for start/stop and a status
// collector for live VRAM/footprint. It is constructed ONLY when
// CITADEL_MODEL_HOTSWAP is on (see buildNodeJobHandlers), so nothing here runs on
// a default node.

// swapController implements worker.SwapController against the live node.
type swapController struct {
	svc            *jobs.ServiceHandler
	configDir      string
	pinnedServices map[string]bool
	logf           func(format string, args ...any)
}

// newSwapController builds the live controller rooted at the node's config +
// workspace dir, honoring the manifest's pinned_services (a pinned engine is
// never swapped out — the #577 allowlist still applies here).
func newSwapController(configDir, workspaceDir string, pinned []string, logf func(string, ...any)) *swapController {
	set := make(map[string]bool, len(pinned))
	for _, p := range pinned {
		if p != "" {
			set[p] = true
		}
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &swapController{
		svc:            jobs.NewServiceHandlerWithWorkspace(configDir, workspaceDir),
		configDir:      configDir,
		pinnedServices: set,
		logf:           logf,
	}
}

// Resident reports whether the engine's container is currently running.
func (c *swapController) Resident(ctx context.Context, backend string) bool {
	for _, e := range status.DiscoverLocalEngines(ctx) {
		if e.Name == backend {
			return true
		}
	}
	return false
}

// PreemptInputs collects the live preemption inputs: running managed serving
// engines other than `exclude` as candidates (VRAM footprint + instantaneous
// idle + pinned flag) and the node's free VRAM. Mirrors the #577 executor's
// buildPreemptCandidates/freeVRAMBytes, but reused here for the NON-durable swap
// path.
func (c *swapController) PreemptInputs(ctx context.Context, exclude string) ([]status.PreemptCandidate, uint64, bool) {
	collector := status.NewCollector(status.CollectorConfig{ConfigDir: c.configDir})
	st, err := collector.Collect()
	if err != nil {
		c.logf("[hotswap] status collect failed; skipping preemption: %v", err)
		return nil, 0, false
	}

	freeVRAM, haveVRAM := freeVRAMBytesFromGPU(st.GPU)

	var candidates []status.PreemptCandidate
	for i := range st.Services {
		s := &st.Services[i]
		if s.Name == exclude || s.Status != status.ServiceStatusRunning {
			continue
		}
		if status.EngineTypeFromName(s.Name) == "" {
			continue // only serving engines are swap candidates
		}
		var vram uint64
		if s.Footprint != nil {
			vram = s.Footprint.VRAMBytes
		}
		candidates = append(candidates, status.PreemptCandidate{
			Name:      s.Name,
			VRAMBytes: vram,
			Idle:      !status.FootprintActive(s.Footprint),
			Pinned:    c.pinnedServices[s.Name],
		})
	}
	return candidates, freeVRAM, haveVRAM
}

// StopNonDurable stops an engine WITHOUT marking desired_status:stopped, so it
// remains eligible to swap back in (StopServiceByName is marker-free — the
// durable marker is set only by the SERVICE_STOP job path).
func (c *swapController) StopNonDurable(name string) error {
	c.logf("[hotswap] evicting %s (non-durable) to free VRAM", name)
	return c.svc.StopServiceByName(name)
}

// Start starts the target engine serving `model` via an internal SERVICE_START
// carrying ONLY {service, model} — deliberately no vram_mb, so #577's DURABLE
// preemptForVRAM stays inert (the swap did its own non-durable preemption). The
// engine's compose-up runs to completion here (may build/load for minutes); the
// caller runs this on a background context, not a job context.
func (c *swapController) Start(ctx context.Context, backend, model string) error {
	c.logf("[hotswap] starting %s (model=%s)", backend, model)
	payload := map[string]string{"service": backend}
	if model != "" {
		payload["model"] = model
	}
	job := &nexus.Job{
		ID:      "hotswap-" + backend,
		Type:    "SERVICE_START",
		Payload: payload,
	}
	jctx := jobs.JobContext{
		Ctx:   ctx,
		LogFn: func(level, msg string) { c.logf("[hotswap] %s", msg) },
	}
	if _, err := c.svc.Execute(jctx, job); err != nil {
		return err
	}
	return nil
}

// Ready reports whether the engine has actually LOADED a model and can serve —
// deliberately stronger than Resident (which is merely "container up"). A vLLM/
// llama.cpp/bonsai/OCR engine only reports a model on /v1/models once its weights
// are loaded and the API is serving, so requiring a non-empty model set here
// avoids the swap declaring "ready" the instant the container exists but before
// the model is loaded (which would route a request into a not-yet-serving engine
// — a hard failure on the bonsai/llama.cpp chat path, which has no readiness
// wait). Ollama is the exception: it lists pulled models before loading any, but
// it auto-loads on the first request, so a listed model is still servable.
func (c *swapController) Ready(ctx context.Context, backend string) bool {
	for _, e := range status.DiscoverLocalEngines(ctx) {
		if e.Name == backend {
			return len(e.Models) > 0
		}
	}
	return false
}

// MeasuredVRAM returns the LIVE measured VRAM footprint (bytes) for the
// now-resident engine `backend`, sourced from the same #421 footprint
// collector PreemptInputs reads for OTHER engines (attachFootprints ->
// ServiceInfo.Footprint.VRAMBytes) — never the engineVRAMEstimateMB table
// (citadel-cli#689). ok is false when the service isn't currently running or
// carries no footprint signal (no GPU, attribution miss); the swap manager
// must not cache a zero as "measured".
func (c *swapController) MeasuredVRAM(ctx context.Context, backend string) (uint64, bool) {
	collector := status.NewCollector(status.CollectorConfig{ConfigDir: c.configDir})
	st, err := collector.Collect()
	if err != nil {
		c.logf("[hotswap] status collect failed measuring %s VRAM: %v", backend, err)
		return 0, false
	}
	for i := range st.Services {
		s := &st.Services[i]
		if s.Name != backend || s.Status != status.ServiceStatusRunning {
			continue
		}
		if s.Footprint != nil && s.Footprint.VRAMBytes > 0 {
			// Log the estimate alongside the measurement so the gap between
			// them (the citadel-cli#689 motivation -- unlimited-ocr measured
			// ~14GB against a 20GB table budget) is visible in the node's
			// logs at the moment it is known, not just applied silently.
			estimateMB := status.EngineVRAMEstimateMB(backend)
			measuredMB := s.Footprint.VRAMBytes / (1024 * 1024)
			c.logf("[hotswap] %s measured VRAM %dMB (table estimate %dMB) -- future swap-ins of this model use the measurement",
				backend, measuredMB, estimateMB)
			return s.Footprint.VRAMBytes, true
		}
	}
	return 0, false
}

// ObserveSwap logs the per-swap record and the running counter that
// citadel-cli#687 asks the node to emit. Before this, an alternating two-model
// workload could spend most of its wall clock loading rather than serving and
// nothing anywhere counted it — the operator saw only "everything is slow".
//
// The eviction ceiling is included on every line so the log says how close the
// node is to refusing, not just what it has done.
func (c *swapController) ObserveSwap(rec worker.SwapRecord, stats worker.SwapStats) {
	evicted := "none"
	if len(rec.Evicted) > 0 {
		evicted = strings.Join(rec.Evicted, ",")
	}
	c.logf("[hotswap] swap %s model=%s outcome=%s evicted=%s wait=%s "+
		"(swaps this hour: %d, evicting: %d/%d)",
		rec.Backend, rec.Model, rec.Outcome, evicted, rec.Wait.Round(time.Second),
		stats.SwapsPerHour, stats.EvictingSwapsPerHour, stats.MaxEvictingPerHour)
}

// freeVRAMBytesFromGPU sums currently-free VRAM (total-used) across all GPUs that
// report a total. The bool is false when NO GPU reports memory, so the caller
// skips the VRAM fit check rather than treat "unknown" as "zero free" (fail-safe,
// mirrors the #577 executor).
func freeVRAMBytesFromGPU(gpus []status.GPUMetrics) (uint64, bool) {
	var free uint64
	found := false
	for _, g := range gpus {
		if g.MemoryTotalMB <= 0 {
			continue
		}
		found = true
		f := g.MemoryTotalMB - g.MemoryUsedMB
		if f < 0 {
			f = 0
		}
		free += uint64(f) * 1024 * 1024
	}
	return free, found
}

// newModelSwapManager builds the swap manager for the node, or nil when hotswap
// is disabled. Returned as worker's swapper so cmd/nodejobs.go can attach it to
// the llm_inference handler only when enabled.
func newModelSwapManager(configDir, workspaceDir string, pinned []string, logf func(string, ...any)) *worker.SwapManager {
	if !status.ModelHotswapEnabled() || configDir == "" {
		return nil
	}
	if logf != nil {
		logf("[hotswap] model hotswap ENABLED (CITADEL_MODEL_HOTSWAP); configDir=%s", configDir)
	}
	// LRU recency (citadel-cli#688) persists under the machine-convergent node
	// config dir (network.GetNodeConfigDir()), NOT the passed-in configDir —
	// that is the invoker-scoped manifest/services dir (platform.ConfigDir()
	// under the hood), which a root systemd worker and an interactive `citadel
	// status` can resolve differently on the same machine (see CLAUDE.md's
	// ConfigDir() note). The swap manager's own restart is what this file
	// exists to survive, so it needs the ONE directory every invocation agrees
	// on, the same reasoning #726's heartbeat freshness marker and worklock use.
	lruPath := filepath.Join(network.GetNodeConfigDir(), worker.LastUsedFileName)
	return worker.NewSwapManager(
		newSwapController(configDir, workspaceDir, pinned, logf),
		worker.WithPersistence(lruPath, logf),
	)
}

// hotswapConfigDir returns configDir when model hotswap is enabled, else "".
// The heartbeat collector runs with ConfigDir="" by default (engines are probed,
// not read from the manifest); hotswap needs the config dir to enumerate
// installed-but-stopped engines, so this passes it through only when enabled —
// leaving the flag-off heartbeat path exactly as it was.
func hotswapConfigDir(configDir string) string {
	if status.ModelHotswapEnabled() {
		return configDir
	}
	return ""
}

// ensure the controller satisfies the interface at compile time.
var _ worker.SwapController = (*swapController)(nil)
