package status

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/engine"
	embeddedservices "github.com/aceteam-ai/citadel-cli/services"
)

// hotswap.go — installed-vs-resident model advertising for VRAM-aware on-demand
// model hotswap (citadel-cli#632).
//
// Hotswap is ON by default. CITADEL_MODEL_HOTSWAP is retained ONLY as a
// break-glass disable: set it to a falsey value (0/false/no/off) to turn the
// swap path off on a node that misbehaves. When disabled, ModelHotswapEnabled()
// returns false and the collector never calls applyModelHotswap, so the
// heartbeat output is byte-identical to the pre-hotswap node.

// ModelHotswapEnabled reports whether VRAM-aware on-demand model hotswap is
// active on this node. Default ON; only an explicit falsey CITADEL_MODEL_HOTSWAP
// (0/false/no/off) disables it (a garbage/unknown value stays ON, so a typo
// can't silently kill the feature). Kept in the leaf status package so both the
// collector and the worker's swap path read the same gate without an import
// cycle.
func ModelHotswapEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CITADEL_MODEL_HOTSWAP"))) {
	case "0", "false", "no", "off":
		return false // break-glass disable
	default:
		return true // default ON (unset or any other value)
	}
}

// engineModelEnvVars maps a managed serving engine to the <name>.env variable(s)
// (in preference order) that select its served model. Mirrors the compose files'
// ${VAR:-default} interpolation so a stopped engine advertises the same model id
// it would serve on start. Engines absent here have no serve-time model env.
//
// citadel #685 slice 2: this is now derived from internal/engine's registry
// (ModelEnvVar per EngineSpec) at init, not a second hand-maintained literal
// -- see internal/engine/tables.go's modelEnvVarsByEngine, the canonical
// table this is a runtime copy of. The llamacpp/bonsai/unlimited-ocr/vllm
// entries and the reasoning behind them (services/compose/llamacpp.yml's
// LLAMACPP_MODEL substitution, citadel-cli#685 §1a) live there now.
var engineModelEnvVars = buildEngineModelEnvVars()

func buildEngineModelEnvVars() map[string][]string {
	out := make(map[string][]string)
	for _, e := range engine.Default().All() {
		if vars := e.Spec().ModelEnvVar; vars != nil {
			cp := make([]string, len(vars))
			copy(cp, vars)
			out[e.Name()] = cp
		}
	}
	return out
}

// EngineModelEnvVars returns a copy of engineModelEnvVars[name] -- the
// <name>.env variable(s) (in preference order) that select the engine's served
// model, or nil when the engine has no such env.
func EngineModelEnvVars(name string) []string {
	vars := engineModelEnvVars[name]
	if vars == nil {
		return nil
	}
	out := make([]string, len(vars))
	copy(out, vars)
	return out
}

// EngineDefaultModel returns engineDefaultModel[name] and whether an entry is
// present. The ok return distinguishes "deliberately no default" (e.g. vllm,
// llamacpp -- present-but-empty would collapse that distinction) from "not in
// the map at all", which citadel #685 §1a identified as exactly the bug a
// naive string return would reintroduce.
func EngineDefaultModel(name string) (string, bool) {
	v, ok := engineDefaultModel[name]
	return v, ok
}

// engineDefaultModel is the served model id an engine falls back to when its
// <name>.env sets no override — the same value the compose ${VAR:-default}
// carries. This is what lets a freshly-installed bonsai/unlimited-ocr advertise a
// concrete model while stopped (the pilot case on node 1297). Engines with no
// stable default (vllm serves whatever weights the deploy selected) are absent,
// so they are advertised only when a model was persisted.
//
// llamacpp is deliberately absent too, for a stronger reason than vllm's: its
// compose has no ${VAR:-default} at all (see llamacpp.yml) — unlike bonsai/
// unlimited-ocr, llamacpp is a bring-your-own-GGUF engine with no single stable
// default file, and with no LLAMACPP_MODEL override set the container starts in
// llama.cpp's own router/deferred-load mode serving no model at all. Advertising
// a fabricated default here would tell the swap path a model is available that
// the engine cannot actually serve, producing a swap that starts llamacpp with
// nothing loaded. resolveInstalledModel("llamacpp") correctly returns "" (no
// swap candidate) until an operator/job persists a real LLAMACPP_MODEL via
// engineModelEnvVars above.
//
// citadel #685 slice 2: derived from internal/engine's registry (DefaultModel
// per EngineSpec) at init -- see internal/engine/tables.go's
// defaultModelByEngine, the canonical table.
var engineDefaultModel = buildEngineDefaultModel()

func buildEngineDefaultModel() map[string]string {
	out := make(map[string]string)
	for _, e := range engine.Default().All() {
		if dm := e.Spec().DefaultModel; dm != nil {
			out[e.Name()] = *dm
		}
	}
	return out
}

// engineVRAMEstimateMB is a per-engine VRAM PROVISIONING BUDGET (MB): the VRAM
// the node should have FREE before it is safe to start the engine, NOT its
// steady-state footprint. It is what a STOPPED installed engine advertises as
// VRAMEstimateMB, and it is the FALLBACK the worker swap planner
// (internal/worker.SwapManager.requiredVRAMBytes) uses only when this node has
// never actually measured the (engine, model) pair being swapped in —
// unavoidable the first time, since nothing can measure a model that isn't
// loaded yet (citadel-cli#689). Once a swap-in completes, the swap planner
// caches and prefers the LIVE measured footprint for that pair over this table
// on every subsequent swap.
//
// Why provisioning budgets, not steady-state, for the fallback: on the RTX
// 3090 pilot (24GB) with Unlimited-OCR resident (~13.5GB), ~10.5GB is free. If
// bonsai's budget were its bounded-context steady-state (~6-8GB) the planner
// would see "fits, no evict" and start bonsai into a card that then can't
// safely hold both — a no-op swap followed by an OOM. Sizing the big engines
// above half the card forces the intended single-big-model-at-a-time swap:
// starting one evicts the other. A RUNNING engine advertises its live
// footprint instead (applyModelHotswap below), so these conservative numbers
// only gate the swap decision, and only until it has been measured once.
//
// citadel #685 slice 2: derived from internal/engine's registry
// (VRAMEstimateMB per EngineSpec) at init -- see internal/engine/tables.go's
// vramEstimateMBByEngine, the canonical table. Lookup semantics (map[string]int,
// zero value for an absent key, no comma-ok) are unchanged, so an engine with
// no entry there still reads 0 here exactly as before.
var engineVRAMEstimateMB = buildEngineVRAMEstimateMB()

func buildEngineVRAMEstimateMB() map[string]int {
	out := make(map[string]int)
	for _, e := range engine.Default().All() {
		if v := e.Spec().VRAMEstimateMB; v != 0 {
			out[e.Name()] = v
		}
	}
	return out
}

// EngineVRAMEstimateMB returns the coarse VRAM estimate (MB) for a managed
// engine, or 0 when unknown. Exported so the worker swap planner can size its
// FALLBACK required-VRAM budget from the same table the heartbeat advertises
// for a STOPPED engine — a measured (engine, model) pair overrides it
// (citadel-cli#689; see the engineVRAMEstimateMB doc comment above).
func EngineVRAMEstimateMB(eng string) int {
	return engineVRAMEstimateMB[eng]
}

// applyModelHotswap annotates the collected status for model hotswap (#632):
//  1. marks every RUNNING managed serving engine Resident=true and attaches its
//     VRAM estimate (live footprint VRAM when known, else the coarse table), and
//  2. additively advertises INSTALLED-but-STOPPED engines (compose materialized
//     on disk, a served model resolvable) as Resident=false swap-in candidates.
//
// reported is the set of service names already present in status.Services (so a
// running engine is not duplicated). Called only when the flag is on.
func (c *Collector) applyModelHotswap(st *NodeStatus, reported map[string]struct{}) {
	// 1. Mark residency + VRAM estimate on already-reported serving engines.
	for i := range st.Services {
		s := &st.Services[i]
		if EngineTypeFromName(s.Name) == "" {
			continue // not a serving engine (db, embedding, misc)
		}
		if s.Status != ServiceStatusRunning {
			continue
		}
		resident := true
		s.Resident = &resident
		if s.VRAMEstimateMB == 0 {
			if s.Footprint != nil && s.Footprint.VRAMBytes > 0 {
				s.VRAMEstimateMB = int(s.Footprint.VRAMBytes / (1024 * 1024))
			} else {
				s.VRAMEstimateMB = engineVRAMEstimateMB[s.Name]
			}
		}
	}

	// 2. Advertise installed-but-stopped engines as swap-in candidates, minus any
	// model a RUNNING service on this node already reports serving. Without that
	// subtraction a stopped vllm's <name>.env default claimed the very model the
	// live tei server was serving, so the platform credited the dead engine and
	// the live one advertised nothing (citadel-cli#690).
	claimed := make(map[string]struct{})
	for i := range st.Services {
		if st.Services[i].Status != ServiceStatusRunning {
			continue
		}
		for _, m := range st.Services[i].Models {
			if m = strings.TrimSpace(m); m != "" {
				claimed[m] = struct{}{}
			}
		}
	}
	for _, eng := range c.collectInstalledEngines(reported, claimed, st.System) {
		st.Services = append(st.Services, eng)
	}
}

// collectInstalledEngines returns installed-but-stopped managed serving engines
// as Resident=false ServiceInfo entries so the platform can route a request to a
// swappable model. An engine is "installed" here when its compose file has been
// materialized on disk (<configDir>/services/<name>.yml) — i.e. it was deployed
// at least once — AND a served model id resolves (persisted <name>.env override
// or the compose default). Engines already in reported (running) are skipped.
// Returns nil when the collector has no configDir (the model source is unknown).
//
// claimed holds the models a RUNNING service on this node already reports. A
// stopped engine whose only resolvable model is in that set is dropped: the
// model is being served, just not by this engine, and advertising it twice let
// the platform attribute it to the stopped one (citadel-cli#690). Pass an empty
// map to advertise unconditionally.
//
// sys is the already-collected system metrics (disk_percent/disk_available_gb)
// used by the disk-headroom preflight clause (citadel-cli#683, swap_preflight.go)
// -- passed in rather than re-collected, since applyModelHotswap's caller already
// populated NodeStatus.System earlier in the same Collect() pass.
func (c *Collector) collectInstalledEngines(reported, claimed map[string]struct{}, sys SystemMetrics) []ServiceInfo {
	if c.configDir == "" {
		return nil
	}
	// Disk headroom is a node-wide signal, evaluated once and reused for every
	// candidate engine below rather than per-engine.
	diskBlocked := diskHeadroomBlocked(sys)
	var out []ServiceInfo
	for name := range embeddedservices.ServiceMap {
		if EngineTypeFromName(name) == "" {
			continue // only serving engines are swappable
		}
		if _, dup := reported[name]; dup {
			continue // already reported (running)
		}
		if !c.engineComposeMaterialized(name) {
			continue // not installed on this node
		}
		model := c.resolveInstalledModel(name)
		if model == "" {
			continue // no advertisable model (e.g. vllm with none persisted)
		}
		if _, taken := claimed[model]; taken {
			continue // a running service on this node already serves it (#690)
		}
		notResident := false
		info := ServiceInfo{
			Name:   name,
			Type:   ServiceTypeLLM,
			Status: ServiceStatusStopped,
			Port:   managedEngineHostPort(name),
			Health: HealthStatusUnknown,
			Models: []string{model},
			// Readiness/Reason (citadel-cli#684): Status/Health above are
			// unchanged (still "stopped"/"unknown") -- this is the disk-only
			// branch, no container is running and no live probe ran, so
			// ProbedAt stays nil (there is nothing to time-stamp: this is a
			// filesystem read, not a protocol probe). ReadinessDown +
			// "installed_not_running" is the exact classification the issue
			// calls out for this branch, distinguishing "we know this engine
			// was deployed here but it is not running" from "we know nothing
			// about this engine at all". This value is unconditional --
			// unrelated to the SwapBlocked* honesty check right below.
			Readiness: ReadinessDown,
			Reason:    "installed_not_running",
			Resident:  &notResident,
		}
		// citadel-cli#683: a compose YAML on disk is a claim about a file, not
		// about serveability. Only an engine that passes ALL THREE checks below
		// gets the VRAM estimate/fast-ETA advertisement; a failing engine still
		// appears (so the platform can render WHY), but carries a
		// machine-readable SwapBlockedReason instead, and no VRAM number --
		// the issue's explicit warning against replacing a wrong boolean with a
		// confidently wrong number.
		switch {
		case !engineImagePresentFn(name):
			info.SwapBlocked = true
			info.SwapBlockedReason = "image_missing"
		case !engineWeightsPresentFn(name):
			info.SwapBlocked = true
			info.SwapBlockedReason = "weights_missing"
		case diskBlocked:
			info.SwapBlocked = true
			info.SwapBlockedReason = "disk_pressure"
		default:
			info.VRAMEstimateMB = engineVRAMEstimateMB[name]
		}
		out = append(out, info)
	}
	return out
}

// engineComposeMaterialized reports whether the engine's compose file exists on
// disk under the collector's config dir (the marker that it was deployed here).
func (c *Collector) engineComposeMaterialized(name string) bool {
	path := filepath.Join(c.configDir, "services", name+".yml")
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// resolveInstalledModel returns the served model id a stopped engine would serve:
// the first present <name>.env override for the engine's model env var(s), else
// the compose default. Empty when neither resolves.
func (c *Collector) resolveInstalledModel(name string) string {
	envPath := filepath.Join(c.configDir, "services", name+".env")
	env := readEnvFile(envPath)
	for _, key := range engineModelEnvVars[name] {
		if v := strings.TrimSpace(env[key]); v != "" {
			return v
		}
	}
	return engineDefaultModel[name]
}

// readEnvFile parses a simple KEY=VALUE .env file into a map. Blank lines,
// comments, and malformed lines are skipped. Surrounding quotes on the value are
// stripped. Returns an empty map when the file is absent or unreadable.
func readEnvFile(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		if k != "" {
			out[k] = v
		}
	}
	return out
}
