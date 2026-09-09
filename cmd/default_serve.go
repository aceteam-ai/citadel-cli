// cmd/default_serve.go
//
// "Default-serve" appliance-mode reconcile (citadel-cli#628). On a truly
// blank GPU node, with the feature explicitly opted in, `citadel work`
// startup auto-serves a VRAM-sized model exactly once, ever, so the box is
// immediately useful without an explicit platform deploy or `citadel run`.
//
// Deliberately does NOT run from `citadel init` -- init only provisions the
// machine and joins the network; whether to auto-serve is a `citadel work`
// startup decision so it applies uniformly regardless of how the node was
// provisioned (init --provision, install.sh, a hand-rolled manifest, ...).
//
// Safety contract (see the four gates in runDefaultServeReconcile and the
// GitHub issue for the full rationale):
//   - default OFF; a node with no opt-in is byte-identical to one without
//     this feature (resolveDefaultServe).
//   - only fires on a genuinely blank node (no serving engine
//     installed/running, no model cached) -- never overrides an existing
//     deploy.
//   - never pins the auto-served service, so #577 preemption / hotswap can
//     evict it later like any other service.
//   - never preempts anything to make room; a VRAM shortfall is a skip, not
//     an eviction.
//   - runs at most once, ever, per node (the on-disk marker) -- including on
//     failure, so a transient docker/network error never turns into a
//     retry-forever loop.
package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cacheindex"
	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/update"
	"github.com/aceteam-ai/citadel-cli/services"
)

// defaultServeMarkerFile is the once-ever completion record. Machine-
// convergent: the caller passes network.GetNodeConfigDir(), not
// platform.ConfigDir() -- see the ConfigDir()-vs-GetNodeConfigDir() note in
// CLAUDE.md for why a root `citadel work` and a later invocation must agree
// on this path.
const defaultServeMarkerFile = "default-serve.json"

// defaultServeMarker records the outcome of the ONE attempt this node will
// ever make. Status is one of "applied", "skipped:<reason>", or
// "failed:<reason>".
type defaultServeMarker struct {
	Status    string    `json:"status"`
	Engine    string    `json:"engine,omitempty"`
	Model     string    `json:"model,omitempty"`
	VRAMMB    int       `json:"vram_mb,omitempty"`
	AppliedAt time.Time `json:"applied_at"`
}

// loadDefaultServeMarker reports whether the once-ever marker is already
// present. A missing or unparseable file is treated as absent (never ran) --
// never as an error that blocks startup.
func loadDefaultServeMarker(configDir string) (*defaultServeMarker, bool) {
	data, err := os.ReadFile(filepath.Join(configDir, defaultServeMarkerFile))
	if err != nil {
		return nil, false
	}
	var m defaultServeMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false
	}
	return &m, true
}

// saveDefaultServeMarker writes the once-ever completion record.
func saveDefaultServeMarker(configDir, status, engine, model string, vramMB int) error {
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	m := defaultServeMarker{
		Status:    status,
		Engine:    engine,
		Model:     model,
		VRAMMB:    vramMB,
		AppliedAt: time.Now().UTC(),
	}
	data, err := json.MarshalIndent(&m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal default-serve marker: %w", err)
	}
	return os.WriteFile(filepath.Join(configDir, defaultServeMarkerFile), data, 0644)
}

// resolveDefaultServe decides whether the default-serve appliance-mode
// reconcile is opted in, mirroring resolveEnergySampling's precedence
// exactly: CITADEL_DEFAULT_SERVE env wins when set, else the manifest's
// default_serve key, else the persisted APPLY_DEVICE_CONFIG-pushed value
// (config.LoadDefaultServe), else OFF.
func resolveDefaultServe(manifest *CitadelManifest) bool {
	if raw := strings.TrimSpace(os.Getenv("CITADEL_DEFAULT_SERVE")); raw != "" {
		return update.IsTruthy(raw)
	}
	if manifest != nil && manifest.DefaultServe {
		return true
	}
	return config.LoadDefaultServe(platform.ConfigDir()).Enabled
}

// defaultServeCandidateEngines is the set of engine names the blank-node gate
// treats as "this node already serves something". Reuses
// services.EngineCacheDirs's key set (the canonical per-engine cache-path
// table, citadel-cli#682/#906) rather than a second hand-maintained list, so
// this gate can never silently diverge from "which embedded engines exist".
func defaultServeCandidateEngines() []string {
	names := make([]string, 0, len(services.EngineCacheDirs))
	for name := range services.EngineCacheDirs {
		names = append(names, name)
	}
	return names
}

// blankNodeCheckForDefaultServe reports whether this node is "blank" for the
// purposes of default-serve: no serving-engine manifest entry, and no
// non-empty cache directory for any embedded engine. Returns (false, reason)
// on the first thing found, so the skip marker can name the concrete cause.
func blankNodeCheckForDefaultServe(manifest *CitadelManifest, cacheRoot string) (bool, string) {
	engines := defaultServeCandidateEngines()

	if manifest != nil {
		candidateSet := make(map[string]struct{}, len(engines))
		for _, e := range engines {
			candidateSet[e] = struct{}{}
		}
		for _, svc := range manifest.Services {
			if _, ok := candidateSet[svc.Name]; ok {
				return false, fmt.Sprintf("service %q already present in manifest", svc.Name)
			}
		}
	}

	// Cache dirs are shared across several engines (e.g. every HF-hub engine
	// mounts the same ~/citadel-cache/huggingface), so dedupe by directory
	// before checking disk -- checking the same directory seven times is
	// wasted work, not wrong, but the dedupe also keeps the skip reason from
	// naming an arbitrary one of several engines sharing a dir.
	checked := make(map[string]bool, len(engines))
	for _, e := range engines {
		cache, ok := services.EngineCacheDirs[e]
		if !ok || checked[cache.Dir] {
			continue
		}
		checked[cache.Dir] = true
		full := filepath.Join(cacheRoot, cache.Dir)
		if dirHasContent(full) {
			return false, fmt.Sprintf("cache directory %q is not empty", full)
		}
	}
	return true, ""
}

// dirHasContent reports whether dir exists and contains at least one entry.
// A missing directory is "no content" (nil error path), not an error --
// exactly what "nothing has ever pulled into this cache dir" looks like on a
// blank node.
func dirHasContent(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	return len(entries) > 0
}

// resolveLargestGPUTotalVRAMMB shells to the platform GPU detector (the same
// primitive internal/status.Collector's collectGPUMetrics uses) and returns
// the largest single detected GPU's total VRAM in MB. found is false when no
// GPU, or no GPU with a known total VRAM, was detected.
func resolveLargestGPUTotalVRAMMB() (mb int, found bool) {
	detector, err := platform.GetGPUDetector()
	if err != nil || !detector.HasGPU() {
		return 0, false
	}
	infos, err := detector.GetGPUInfo()
	if err != nil {
		return 0, false
	}
	metrics := make([]status.GPUMetrics, 0, len(infos))
	for _, gpu := range infos {
		memStr := strings.TrimSuffix(strings.TrimSuffix(gpu.Memory, " MB"), "MB")
		v, err := strconv.Atoi(strings.TrimSpace(memStr))
		if err != nil {
			continue
		}
		metrics = append(metrics, status.GPUMetrics{MemoryTotalMB: v})
	}
	return status.LargestGPUTotalVRAMMB(metrics)
}

// defaultServeDeps groups every side-effecting call the reconcile makes
// behind function values, so the decision logic (runDefaultServeReconcile)
// is unit-testable without a real GPU, docker daemon, or ollama binary.
type defaultServeDeps struct {
	largestGPUTotalVRAMMB func() (mb int, found bool)
	// executeServiceStart synthesizes the exact steps a platform
	// SERVICE_START {service, model} dispatch performs: materialize the
	// embedded compose file if needed, additively register the service in
	// citadel.yaml (desired_status left empty, i.e. "start on boot" -- see
	// cmd/manifest.go Service.DesiredStatus), and start it -- reusing
	// jobs.ServiceHandler.Execute exactly, not a second implementation.
	executeServiceStart func(engine, model string) error
	log                 func(format string, args ...any)
}

// realDefaultServeDeps builds the production defaultServeDeps, routing
// executeServiceStart through the given ServiceHandler via a synthetic
// SERVICE_START job -- the same handler cmd/work.go's reservation reconcile
// (citadel-cli#832) already constructs at this point in startup
// (reservationHandler), so no second ServiceHandler is created.
func realDefaultServeDeps(handler *jobs.ServiceHandler) defaultServeDeps {
	return defaultServeDeps{
		largestGPUTotalVRAMMB: resolveLargestGPUTotalVRAMMB,
		executeServiceStart: func(engine, model string) error {
			payload := map[string]string{"service": engine}
			if model != "" {
				payload["model"] = model
			}
			job := &nexus.Job{ID: "default-serve", Type: "SERVICE_START", Payload: payload}
			_, err := handler.Execute(jobs.JobContext{LogFn: func(_, msg string) { Log("%s", msg) }}, job)
			return err
		},
		log: Log,
	}
}

// runDefaultServeReconcile is the entire default-serve decision, called once
// from runWork's startup (see the call site there for exactly where and why).
// It is a complete no-op -- no file I/O beyond the opt-in check itself, no
// marker written -- unless opted in; see resolveDefaultServe.
func runDefaultServeReconcile(manifest *CitadelManifest, nodeConfigDir string, deps defaultServeDeps) {
	if !resolveDefaultServe(manifest) {
		// Not opted in: byte-identical to a node without this feature. No
		// marker written, so a LATER opt-in (env, manifest edit, or an
		// APPLY_DEVICE_CONFIG push) still gets its one chance on a
		// subsequent boot.
		return
	}
	if _, ok := loadDefaultServeMarker(nodeConfigDir); ok {
		deps.log("default-serve: already attempted on this node (see %s); skipping", filepath.Join(nodeConfigDir, defaultServeMarkerFile))
		return
	}

	vramMB, found := deps.largestGPUTotalVRAMMB()
	if !found || vramMB <= 0 {
		deps.log("default-serve: opted in, but no GPU with known VRAM detected; skipping (will not retry)")
		if err := saveDefaultServeMarker(nodeConfigDir, "skipped:no-gpu", "", "", 0); err != nil {
			deps.log("default-serve: warning: failed to write completion marker: %v", err)
		}
		return
	}

	blank, reason := blankNodeCheckForDefaultServe(manifest, cacheindex.DefaultCacheRoot())
	if !blank {
		deps.log("default-serve: opted in, but node is not blank (%s); skipping (will not retry)", reason)
		if err := saveDefaultServeMarker(nodeConfigDir, "skipped:not-blank: "+reason, "", "", vramMB); err != nil {
			deps.log("default-serve: warning: failed to write completion marker: %v", err)
		}
		return
	}

	engine, model := status.DefaultServeTier(vramMB)
	if engine == "" {
		deps.log("default-serve: opted in, but %d MB VRAM matched no tier; skipping (will not retry)", vramMB)
		if err := saveDefaultServeMarker(nodeConfigDir, "skipped:no-tier-match", "", "", vramMB); err != nil {
			deps.log("default-serve: warning: failed to write completion marker: %v", err)
		}
		return
	}

	target := engine
	if model != "" {
		target = fmt.Sprintf("%s (%s)", engine, model)
	}
	deps.log("default-serve: appliance mode auto-serving %s on this blank %d MB-VRAM GPU node (citadel-cli#628, opt-in; see 'citadel run --service %s' or the AceTeam dashboard to change it later)", target, vramMB, engine)

	if err := deps.executeServiceStart(engine, model); err != nil {
		deps.log("default-serve: FAILED to auto-serve %s: %v (will not retry -- see %s)", target, err, filepath.Join(nodeConfigDir, defaultServeMarkerFile))
		if mErr := saveDefaultServeMarker(nodeConfigDir, "failed: "+err.Error(), engine, model, vramMB); mErr != nil {
			deps.log("default-serve: warning: failed to write completion marker: %v", mErr)
		}
		return
	}

	deps.log("default-serve: applied -- now serving %s", target)
	if err := saveDefaultServeMarker(nodeConfigDir, "applied", engine, model, vramMB); err != nil {
		deps.log("default-serve: warning: failed to write completion marker: %v", err)
	}
}
