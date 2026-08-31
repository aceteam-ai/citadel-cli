// internal/jobs/meeting_vram_diagnosis.go
//
// Fail-fast / honest-refusal tier of citadel#891 (design:
// docs/design-meeting-vram-coresidency.md). Two independent pieces, both
// scoped to MEETING_JOIN and TRANSCRIBE_AUDIO:
//
//  1. Always-on readiness-failure diagnosis (§4a): when the whisper sidecar
//     never becomes ready, annotate the resulting error with the node's free
//     VRAM/RAM and current top resource holders, so a "did not become ready"
//     failure stops being silent about WHY -- regardless of whether the true
//     cause is VRAM contention (the issue's own framing), host-RAM pressure
//     (the design doc's leading suspect for the original node-1084 incident
//     -- see the doc's §1a/§7 Q1), or something else entirely (a crashed or
//     restart-looping container). Changes only an error STRING; never a
//     success/failure outcome.
//  2. Payload-declared VRAM preflight (§2/§3/§5): refuse a MEETING_JOIN or
//     TRANSCRIBE_AUDIO dispatch fast, with a structured reason, when the
//     payload declares a vram_mb/vram_gb budget this node's currently-free
//     VRAM cannot satisfy. Deliberately payload-only and UNGATED by
//     CITADEL_RESOURCE_ISOLATION -- mirrors how #577's own payload-declared
//     vram_mb already acts ungated on SERVICE_START (see
//     resolveRequiredVRAMBytes's doc comment in service_handler.go). The
//     design doc's citadel-side ESTIMATE fallback (§3 option 2, gated behind
//     CITADEL_RESOURCE_ISOLATION) is deliberately NOT built here: the shipped
//     services/compose/transcribe.yml hardcodes WHISPER_DEVICE=cpu directly
//     in its `environment:` list with no operator-configurable override
//     (unlike llamacpp's <name>.env sibling-file idiom), so an estimator
//     reading "the materialized transcribe compose/env" would always read
//     that same hardcoded cpu value and return 0 -- provably dead code today.
//     Building it (once a transcribe-gpu compose variant or an env-override
//     mechanism exists) is a documented follow-up, not skipped silently.
//
// Both are INERT on the shipped node config: the backend does not send
// vram_mb/vram_gb on either job type today (same "inert until the backend
// forwards a field" posture as #577's original landing), and the shipped
// whisper sidecar is CPU-only (0 VRAM need), so neither piece changes any
// currently-working meeting/transcribe job's outcome.
package jobs

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// ReasonInsufficientVRAM is the machine-readable refusal code a
// checkVRAMPreflight refusal carries, so the backend can branch on it
// (fall back to cloud transcription, re-dispatch to another node, surface a
// clear message) without parsing a human sentence. Mirrors the
// swap_rate_limited precedent (internal/worker/swap.go) for a structured
// job-failure reason.
const ReasonInsufficientVRAM = "insufficient_vram"

// VRAMRefusal is the typed error returned when a job's declared vram_mb/
// vram_gb budget cannot fit on this node. Its Error() renders a
// machine-readable JSON object {"reason":"...","message":"..."}, mirroring
// ShellRefusal's convention (shell_command.go) exactly -- LegacyHandlerAdapter
// surfaces a legacy handler's err.Error() verbatim as Output["error"], so
// this is the established way a legacy jobs.JobHandler communicates a
// structured reason through that path today.
type VRAMRefusal struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// Error renders the refusal as a JSON object. Reason and Message are plain
// strings so json.Marshal cannot fail here; the concatenated fallback exists
// only to satisfy the error contract defensively.
func (e *VRAMRefusal) Error() string {
	b, err := json.Marshal(e)
	if err != nil {
		return `{"reason":"` + e.Reason + `","message":"insufficient VRAM"}`
	}
	return string(b)
}

// collectStatusFn is the shape both the diagnosis and preflight pieces
// collect live node status through -- a nil value means "no status source
// configured" and both pieces skip cleanly (fail open: no annotation, no
// refusal), never fail closed on missing observability.
type collectStatusFn func() (*status.NodeStatus, error)

// checkVRAMPreflight refuses a MEETING_JOIN/TRANSCRIBE_AUDIO dispatch whose
// payload declares a vram_mb/vram_gb budget (parseRequiredVRAMBytes, reused
// verbatim from service_handler.go) that this node's currently-free VRAM
// cannot satisfy. See the package doc above for the full contract and why
// this is payload-only / ungated.
//
// Contract, mirroring PlanRAMPreflight/PlanPreemption's fail-open/
// fail-closed posture exactly:
//   - No vram_mb/vram_gb declared (true on every real dispatch today) =>
//     proceed, no check performed at all.
//   - collect is nil (handler has no ConfigDir, e.g. hermetic construction)
//     => proceed; a preflight cannot run without a status source.
//   - Free VRAM unknown (no GPU / nvidia-smi absent / collection error) =>
//     proceed + log (fail OPEN -- a preflight is not a guaranteed hold).
//   - Declared requirement fits in currently-free VRAM => proceed.
//   - Confirmed shortfall => refuse with *VRAMRefusal{Reason:
//     ReasonInsufficientVRAM}, naming the current top VRAM holders.
func checkVRAMPreflight(ctx JobContext, payload map[string]string, collect collectStatusFn) error {
	required := parseRequiredVRAMBytes(payload)
	if required == 0 || collect == nil {
		return nil
	}
	st, err := collect()
	if err != nil {
		ctx.Log("warn", "VRAM preflight: could not collect node status, proceeding: %v", err)
		return nil
	}
	available, known := freeVRAMBytes(st.GPU)
	if !known {
		ctx.Log("warn", "VRAM preflight: no GPU VRAM signal on this node, proceeding")
		return nil
	}
	if available >= required {
		return nil
	}
	msg := fmt.Sprintf("insufficient VRAM: needs %s, node has %s free", fmtGBBytes(required), fmtGBBytes(available))
	if holders := topVRAMHolders(st.Services); holders != "" {
		msg += "; holding: " + holders
	}
	return &VRAMRefusal{Reason: ReasonInsufficientVRAM, Message: msg}
}

// diagnoseReadinessFailure builds an appended annotation for a
// waitForReady-style timeout/unreachable error, naming free GPU VRAM, free
// RAM, and the current top resource holders. Pure formatting over an
// already-collected status.NodeStatus (never nil-panics: a nil st or an
// empty result both just produce no annotation), so this never becomes a
// second steady-state docker/nvidia-smi sweep -- it only runs on an
// already-slow failure path, once, via the caller's collect function.
func diagnoseReadinessFailure(st *status.NodeStatus) string {
	if st == nil {
		return ""
	}
	var parts []string
	if free, known := freeVRAMBytes(st.GPU); known {
		var total uint64
		for _, g := range st.GPU {
			if g.MemoryTotalMB > 0 {
				total += uint64(g.MemoryTotalMB) * 1024 * 1024
			}
		}
		parts = append(parts, fmt.Sprintf("gpu: %s free of %s", fmtGBBytes(free), fmtGBBytes(total)))
	}
	if st.System.MemoryTotalGB > 0 {
		parts = append(parts, fmt.Sprintf("ram: %.1fGB available of %.1fGB", st.System.MemoryAvailableGB, st.System.MemoryTotalGB))
	}
	if holders := topResourceHolders(st.Services); holders != "" {
		parts = append(parts, "top holders: "+holders)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

// topVRAMHolders names up to 3 running services by VRAM footprint, largest
// first, e.g. "vllm 21.2GB, bonsai 3.8GB". Services with no footprint or a
// zero VRAM reading are omitted. Returns "" when nothing qualifies.
func topVRAMHolders(services []status.ServiceInfo) string {
	type holder struct {
		name  string
		bytes uint64
	}
	var holders []holder
	for _, s := range services {
		if s.Status != status.ServiceStatusRunning || s.Footprint == nil || s.Footprint.VRAMBytes == 0 {
			continue
		}
		holders = append(holders, holder{name: s.Name, bytes: s.Footprint.VRAMBytes})
	}
	sort.SliceStable(holders, func(i, j int) bool { return holders[i].bytes > holders[j].bytes })
	if len(holders) > 3 {
		holders = holders[:3]
	}
	parts := make([]string, 0, len(holders))
	for _, h := range holders {
		parts = append(parts, fmt.Sprintf("%s %s", h.name, fmtGBBytes(h.bytes)))
	}
	return strings.Join(parts, ", ")
}

// topResourceHolders names up to 3 running services by VRAM footprint
// (topVRAMHolders); when no service reports any VRAM at all (a CPU-only
// node -- the shipped configuration), it falls back to the same ranking by
// RAM footprint instead, so the diagnosis annotation stays useful on exactly
// the config where the design doc's leading incident hypothesis (host-RAM
// thrash, not VRAM) applies.
func topResourceHolders(services []status.ServiceInfo) string {
	if v := topVRAMHolders(services); v != "" {
		return v
	}
	type holder struct {
		name  string
		bytes uint64
	}
	var holders []holder
	for _, s := range services {
		if s.Status != status.ServiceStatusRunning || s.Footprint == nil || s.Footprint.RAMBytes == 0 {
			continue
		}
		holders = append(holders, holder{name: s.Name, bytes: s.Footprint.RAMBytes})
	}
	sort.SliceStable(holders, func(i, j int) bool { return holders[i].bytes > holders[j].bytes })
	if len(holders) > 3 {
		holders = holders[:3]
	}
	parts := make([]string, 0, len(holders))
	for _, h := range holders {
		parts = append(parts, fmt.Sprintf("%s %s ram", h.name, fmtGBBytes(h.bytes)))
	}
	return strings.Join(parts, ", ")
}

// fmtGBBytes renders a byte count as a compact "5.5GB" for diagnostic/refusal
// messages, matching status.fmtGB's format (unexported in that package, so
// mirrored here rather than imported).
func fmtGBBytes(b uint64) string {
	return fmt.Sprintf("%.1fGB", float64(b)/(1<<30))
}
