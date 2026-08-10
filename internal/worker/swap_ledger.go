// internal/worker/swap_ledger.go
//
// Swap accounting and the swap rate bound (citadel-cli#687).
//
// A one-GPU node running an alternating two-model workload can spend most of its
// wall clock loading rather than serving, and before this nothing counted it:
// there was no swap counter, no rate limit, and no per-swap record. The symptom
// reaching the user is "everything is slow", which is unactionable.
//
// So swaps are recorded, and the EVICTING ones are bounded. Two deliberate
// choices:
//
//   - The bound counts swaps that actually EVICTED a resident engine, not every
//     swap. Starting an engine into free VRAM costs a load but takes nothing
//     away, so a roomy node is never rate-limited for using its own headroom.
//     Thrash is specifically the evict-reload-evict cycle.
//   - Hitting the ceiling REFUSES, it does not queue or degrade quietly. A user
//     told "this node is at its swap limit" can make a decision; a user watching
//     an unbounded thrash cannot.
//
// The ledger is in-process. It resets on restart, so a crash-looping worker can
// still exceed the ceiling across restarts — persisting it is out of scope here
// and belongs with the persisted `lastUsed` work in citadel-cli#688.
package worker

import (
	"fmt"
	"time"
)

// Swap accounting knobs. Vars, not consts, so tests can shrink the window
// without sleeping through an hour; swapAccountingDefaults pins the shipped
// values so a test tweak cannot silently become the default.
var (
	// swapRateWindow is the trailing window both the rate bound and the
	// swaps-per-hour counter measure over.
	swapRateWindow = time.Hour

	// swapMaxEvictingPerWindow is how many evicting swaps a node will perform per
	// window before refusing. Six is roughly "a swap every ten minutes": enough
	// for a node genuinely serving several models in rotation, low enough that an
	// alternating two-model workload hits it quickly instead of burning the box
	// for hours.
	swapMaxEvictingPerWindow = 6

	// swapRecordsKept bounds the in-process ledger so a long-lived worker does
	// not accumulate records forever.
	swapRecordsKept = 64
)

// Swap outcomes recorded in the ledger.
const (
	swapOutcomeReady       = "ready"        // engine loaded and became ready
	swapOutcomeWarming     = "warming"      // start issued; not ready before the background ceiling
	swapOutcomeFailed      = "failed"       // the start (or an eviction) errored
	swapOutcomeBlocked     = "blocked"      // could not proceed now (residency protection); nothing started
	swapOutcomeRateLimited = "rate_limited" // refused by the swap rate bound; nothing started
)

// SwapRecord is one swap this node attempted. It records what the manager
// genuinely knows.
//
// Deliberately absent: "whether a pull was required" (asked for in #687). The
// manager issues a SERVICE_START and the weights pull, if any, happens inside
// it — the manager cannot observe it, and a guessed field is worse than no
// field. Reporting it needs the start path to report back; tracked separately.
type SwapRecord struct {
	// Backend is the engine swapped in.
	Backend string `json:"backend"`
	// Model is the model it was started for.
	Model string `json:"model,omitempty"`
	// Evicted names the engines stopped to make room, in stop order. Empty when
	// the swap fit in free VRAM — the distinction the rate bound keys on.
	Evicted []string `json:"evicted,omitempty"`
	// StartedAt is when the swap began.
	StartedAt time.Time `json:"started_at"`
	// Wait is how long the swap ran before reaching its outcome.
	Wait time.Duration `json:"wait"`
	// Outcome is one of the swapOutcome* values.
	Outcome string `json:"outcome"`
}

// Evicting reports whether this swap took VRAM away from a resident engine.
func (r SwapRecord) Evicting() bool { return len(r.Evicted) > 0 }

// SwapStats is the operator-facing view of swap activity: the counter #687 asks
// for, plus the records behind it so a "why is this node slow" question has an
// answer instead of a number.
type SwapStats struct {
	// SwapsPerHour counts every swap attempt in the trailing window.
	SwapsPerHour int `json:"swaps_per_hour"`
	// EvictingSwapsPerHour counts only those that stopped a resident engine —
	// the subset the rate bound applies to.
	EvictingSwapsPerHour int `json:"evicting_swaps_per_hour"`
	// MaxEvictingPerHour is the ceiling in force, so a reader can tell how close
	// to refusing the node is without knowing the build's defaults.
	MaxEvictingPerHour int `json:"max_evicting_per_hour"`
	// Recent holds the most recent records, oldest first.
	Recent []SwapRecord `json:"recent,omitempty"`
}

// SwapRateLimitedError is returned when a swap would have to evict a resident
// engine and the node has already done so its full allowance this window. It is
// a hard error on purpose: the inference handler turns it into a job failure
// naming the limit, rather than a warming result that invites a retry into the
// same refusal.
type SwapRateLimitedError struct {
	Backend string
	Swaps   int
	Max     int
	Window  time.Duration
}

func (e *SwapRateLimitedError) Error() string {
	return fmt.Sprintf(
		"cannot swap in %s: this node is at its swap limit (%d evicting swaps in the last %s, limit %d). "+
			"Loading another model would evict a resident one and spend more time loading than serving; "+
			"refusing instead of thrashing (citadel-cli#687)",
		e.Backend, e.Swaps, e.Window, e.Max)
}

// swapObserver is an OPTIONAL SwapController capability. A controller that
// implements it is handed each completed swap record, which is how the per-swap
// record and the counter reach the node's logs. Controllers that do not
// implement it are unaffected.
type swapObserver interface {
	ObserveSwap(rec SwapRecord, stats SwapStats)
}

// recordSwap appends a completed swap to the ledger, prunes anything outside the
// window (and beyond the retention bound), and hands the record plus the
// resulting counters to the controller if it observes swaps.
func (m *SwapManager) recordSwap(rec SwapRecord) {
	m.mu.Lock()
	m.swaps = append(m.swaps, rec)
	m.pruneSwapsLocked(m.now())
	stats := m.swapStatsLocked(m.now())
	m.mu.Unlock()

	if obs, ok := m.ctrl.(swapObserver); ok {
		obs.ObserveSwap(rec, stats)
	}
}

// pruneSwapsLocked drops records older than the rate window, then caps the slice
// at the retention bound. Callers hold m.mu.
func (m *SwapManager) pruneSwapsLocked(now time.Time) {
	cutoff := now.Add(-m.rateWindow)
	keep := m.swaps[:0]
	for _, r := range m.swaps {
		if r.StartedAt.After(cutoff) {
			keep = append(keep, r)
		}
	}
	m.swaps = keep
	if len(m.swaps) > swapRecordsKept {
		m.swaps = append([]SwapRecord(nil), m.swaps[len(m.swaps)-swapRecordsKept:]...)
	}
}

// evictingSwapsInWindow counts swaps in the trailing window that stopped a
// resident engine. Callers hold m.mu.
func (m *SwapManager) evictingSwapsInWindowLocked(now time.Time) int {
	cutoff := now.Add(-m.rateWindow)
	n := 0
	for _, r := range m.swaps {
		if r.Evicting() && r.StartedAt.After(cutoff) {
			n++
		}
	}
	return n
}

// swapStatsLocked builds the counter view. Callers hold m.mu.
func (m *SwapManager) swapStatsLocked(now time.Time) SwapStats {
	cutoff := now.Add(-m.rateWindow)
	stats := SwapStats{MaxEvictingPerHour: m.maxEvictingPerWindow}
	for _, r := range m.swaps {
		if !r.StartedAt.After(cutoff) {
			continue
		}
		stats.SwapsPerHour++
		if r.Evicting() {
			stats.EvictingSwapsPerHour++
		}
	}
	stats.Recent = append([]SwapRecord(nil), m.swaps...)
	return stats
}

// SwapStats reports swap activity over the trailing window. It is the read side
// of the ledger the rate bound writes.
func (m *SwapManager) SwapStats() SwapStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.swapStatsLocked(m.now())
}
