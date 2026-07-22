// Package pulse collects node-local inference and GPU telemetry for Fabric
// Pulse (aceteam-ai/aceteam#6334, citadel-cli#587).
//
// The sovereign path: Citadel scrapes local vLLM / SGLang Prometheus /metrics
// endpoints and nvidia-smi, condenses them into a compact versioned stats
// block, and ships that block on the existing heartbeat. The backend never
// opens a socket into the node (Railway userspace-tailscaled cannot anyway;
// see fabric-vpn-relay docs).
//
// Wire contract (field names are load-bearing — the aceteam backend/web are
// built against this exact shape):
//
//	"stats": {
//	  "v": 1,
//	  "ts": 1753142400,
//	  "gpus": [{"i": 0, "util_pct": 85, "mem_used_mb": 22528, ...}],
//	  "inference": [{"engine": "vllm", "model": "Qwen3-9B", "port": 8080, ...}]
//	}
//
// Every field inside a gpus[]/inference[] entry is optional-when-unavailable:
// a metric an engine does not expose is omitted, never zero-filled. The gpus/
// inference arrays are omitted entirely when the node has no GPU / no running
// inference engine. The whole stats block is optional on the heartbeat, so old
// backends ignore it and old nodes keep heartbeating without it.
package pulse

// StatsVersion is the wire version of the stats block ("v" field). Bump only
// on a breaking shape change; additive fields do not need a bump.
const StatsVersion = 1

// StatsBlock is the compact per-heartbeat stats payload. V and TS are always
// present; both arrays are omitted when the collector has nothing to report.
type StatsBlock struct {
	// V is the stats block wire version (StatsVersion).
	V int `json:"v"`
	// TS is the Unix timestamp (seconds) of the collection.
	TS int64 `json:"ts"`
	// GPUs holds per-GPU utilization, sourced from nvidia-smi. Omitted when
	// no NVIDIA GPU / driver is present.
	GPUs []GPUStat `json:"gpus,omitempty"`
	// Inference holds per-engine internals scraped from local Prometheus
	// /metrics endpoints. Omitted when no inference engine is reachable.
	Inference []InferenceStat `json:"inference,omitempty"`
}

// GPUStat is one GPU's live utilization snapshot. All fields except the index
// are pointers so a value nvidia-smi reports as "[N/A]"/"[Not Supported]" is
// omitted from the JSON instead of shipping a fake zero.
type GPUStat struct {
	// Index is the GPU index as reported by nvidia-smi (always present).
	Index      int      `json:"i"`
	UtilPct    *float64 `json:"util_pct,omitempty"`
	MemUsedMB  *int     `json:"mem_used_mb,omitempty"`
	MemTotalMB *int     `json:"mem_total_mb,omitempty"`
	TempC      *int     `json:"temp_c,omitempty"`
	PowerW     *float64 `json:"power_w,omitempty"`
}

// InferenceStat is one engine's live internals. Engine and Port are always
// present; every metric is a pointer so a signal the engine does not expose
// (or that has no baseline yet, for rate/percentile fields) is omitted rather
// than zero-filled. A present zero (e.g. waiting=0) is real and is shipped.
type InferenceStat struct {
	// Engine is the metrics dialect that produced this entry ("vllm", "sglang").
	Engine string `json:"engine"`
	// Model is the served model name, from the model_name label when the
	// engine attaches one to its series. Omitted when unlabeled.
	Model string `json:"model,omitempty"`
	// Port is the local host port the metrics were scraped from.
	Port int `json:"port"`
	// GenTPS / PromptTPS are generation / prompt token throughput in tokens
	// per second: the engine's own throughput gauge when exposed, otherwise a
	// rate computed from token counters across consecutive scrapes (omitted on
	// the first scrape, which has no baseline).
	GenTPS    *float64 `json:"gen_tps,omitempty"`
	PromptTPS *float64 `json:"prompt_tps,omitempty"`
	// KVCachePct is KV-cache utilization as a percentage (engines report a
	// 0-1 fraction; scaled here).
	KVCachePct *float64 `json:"kv_cache_pct,omitempty"`
	// Running / Waiting are the engine's live request queue gauges.
	Running *int `json:"running,omitempty"`
	Waiting *int `json:"waiting,omitempty"`
	// TTFTMsP50 / E2EMsP50 are median time-to-first-token / end-to-end request
	// latency in milliseconds, computed from the engine's Prometheus histogram
	// over the window between consecutive scrapes. Omitted when the engine
	// exposes no histogram or no request finished in the window.
	TTFTMsP50 *float64 `json:"ttft_ms_p50,omitempty"`
	E2EMsP50  *float64 `json:"e2e_ms_p50,omitempty"`
}

// float64Ptr / intPtr are tiny helpers for building optional fields.
func float64Ptr(v float64) *float64 { return &v }
func intPtr(v int) *int             { return &v }
