// internal/worker/swap_preflight.go
//
// Wires citadel-cli#955's serveability preflight into the node's OWN on-demand
// swap decision (citadel-cli#956, follow-up to #683/#955).
//
// #955 made the heartbeat's advertisement honest: an engine whose image was
// Docker-GC'd under disk pressure now reports swap_blocked instead of
// advertising a fast warm-on-demand swap. But that only fixed what the node
// TELLS the platform -- SwapManager.EnsureResident (swap.go) still attempted a
// swap start with no preflight at all, so an inference request for an
// installed-but-absent engine could still trigger a full Start() -> multi-GB
// pull that exceeds swapBackgroundMaxDur or dies on "no space left on
// device". This file closes that gap: the SAME three checks, reused (not
// reimplemented) via status.EngineServeablePreflight, run once at the top of
// runSwap -- i.e. exactly on the path about to start a NEW engine, never on
// the resident-hit fast path (EnsureResident's ctrl.Resident check already
// returns before startOrJoin/runSwap are reached in that case).
//
// This is defense-in-depth, not a replacement for the heartbeat fix: the
// image/weights/disk state can change in the race between a heartbeat tick
// and a dispatch, and until the aceteam scheduler consumes SwapBlockedReason
// (separate, cross-repo work) the node's own swap path is the only place that
// can fail fast on it.
package worker

import (
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// SwapPreflightBlockedError is returned when a swap needs to START a new
// engine and status.EngineServeablePreflight finds a genuine, positively-
// classified absence (image GC'd, weights swept, disk pressure) -- NOT a
// "couldn't determine" case, which fails open and never reaches this error
// (see EngineServeablePreflight's doc comment for the exact fail-open
// contract this relies on and must not weaken).
//
// It is a hard error on purpose, mirroring SwapRateLimitedError's posture
// (swap_ledger.go): the inference handler turns it into a job FAILURE naming
// the reason, never a model_warming success -- warming promises imminent
// readiness, which is false when the image was GC'd or the disk is full, and
// reporting it anyway would burn the wait budget on a pull that was always
// going to fail instead of surfacing an actionable error immediately.
type SwapPreflightBlockedError struct {
	Backend string
	Reason  string // one of "image_missing" | "weights_missing" | "disk_pressure"
}

func (e *SwapPreflightBlockedError) Error() string {
	return fmt.Sprintf(
		"cannot swap in %s: not serveable (%s) -- refusing rather than attempting a doomed pull (citadel-cli#956)",
		e.Backend, e.Reason)
}

// defaultSwapPreflight is SwapManager.preflight's production wiring: the real
// status.EngineServeablePreflight, reusing #955's image/weights/disk checks
// unmodified.
func defaultSwapPreflight(backend string, sys status.SystemMetrics) (blocked bool, reason string) {
	return status.EngineServeablePreflight(backend, sys)
}
