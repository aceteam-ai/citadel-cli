// Package telemetry streams the Citadel node's activity feed to the AceTeam
// control plane for remote debugging.
//
// It reuses the same authenticated backend channel the node already uses for
// heartbeats: events are written to a Redis stream via the device-token-authed
// Redis API proxy (redisapi.Client.StreamAdd), NOT to Supabase directly. A
// Python worker is expected to drain the stream into the events log.
//
// Design constraints (issue #294):
//   - Anonymous: payloads carry only node/debug context, never user PII.
//   - Opt-out: a persisted config flag (anon_telemetry_enabled, default true)
//     gates ALL emission; the flag is re-read per emit so a runtime toggle from
//     the settings pane (#295) takes effect without restart.
//   - Crash-safe + fire-and-forget: emission must never block or panic the TUI.
//     Emit() spawns a guarded goroutine with recover; failures are dropped.
package telemetry

import (
	"context"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

// activityStream is the Redis stream that activity events are written to.
// A backend consumer must drain this into the events log (see PR notes).
const activityStream = "node:activity:stream"

// streamMaxLen bounds the stream so it self-trims on the proxy side.
const streamMaxLen = 10000

// emitTimeout bounds a single emission so a slow/hung backend can never wedge
// the emitter goroutine indefinitely.
const emitTimeout = 5 * time.Second

// eventSink is the minimal surface the emitter needs from the Redis API client.
// *redisapi.Client satisfies it; tests inject a fake.
type eventSink interface {
	StreamAdd(ctx context.Context, stream string, values map[string]string, maxLen int64) error
}

// emitter holds the configured streaming context. Fields are immutable after
// Configure; nodeID/orgID/etc. are node/debug context only (no user PII).
type emitter struct {
	sink            eventSink
	configDir       string // where telemetry.yaml lives; the flag is re-read from here per emit
	nodeID          string
	headscaleNodeID string
	orgID           string
	version         string
}

var (
	mu      sync.RWMutex
	current *emitter
)

// Configure wires up activity streaming. Called once the Redis API client and
// node/debug context are available (in ccStartWorker, alongside the heartbeat
// publisher). Before Configure runs, Emit is a silent no-op, so early
// pre-worker activity simply isn't streamed.
func Configure(sink eventSink, configDir, nodeID, headscaleNodeID, orgID, version string) {
	mu.Lock()
	defer mu.Unlock()
	if sink == nil {
		current = nil
		return
	}
	current = &emitter{
		sink:            sink,
		configDir:       configDir,
		nodeID:          nodeID,
		headscaleNodeID: headscaleNodeID,
		orgID:           orgID,
		version:         version,
	}
}

// Reset clears the configured emitter. Primarily for tests.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	current = nil
}

// Emit streams a single activity entry. It is fire-and-forget and crash-safe:
// it never blocks the caller and never panics into the caller's goroutine.
// All work (flag check, payload build, network) happens off-thread.
func Emit(level, message string) {
	mu.RLock()
	e := current
	mu.RUnlock()
	if e == nil {
		return
	}
	go func() {
		defer func() {
			// Telemetry must never crash the TUI; swallow any panic.
			_ = recover()
		}()
		e.emit(level, message)
	}()
}

// emit performs one synchronous emission attempt: gate on the flag, build the
// anonymous payload, and write to the stream. It is separated from Emit so the
// gating and anonymization can be tested deterministically (no goroutine).
// Failures are intentionally dropped — telemetry is best-effort.
func (e *emitter) emit(level, message string) {
	// Re-read the opt-out flag per emit so the settings pane (#295) toggle takes
	// effect at runtime without a restart. The read is off the TUI thread.
	if !config.LoadTelemetry(e.configDir).AnonTelemetryEnabled {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), emitTimeout)
	defer cancel()

	// buildPayload deliberately includes only node/debug context. There is no
	// user-identity field (email/name) anywhere in this map.
	_ = e.sink.StreamAdd(ctx, activityStream, e.buildPayload(level, message), streamMaxLen)
}

// buildPayload constructs the anonymous stream fields for an activity entry.
// It is exported-for-test via emit; kept as a method so the field set is in one
// place and obviously PII-free. Only node/debug context is included.
func (e *emitter) buildPayload(level, message string) map[string]string {
	return map[string]string{
		"nodeId":          e.nodeID,
		"headscaleNodeId": e.headscaleNodeID,
		"orgId":           e.orgID,
		"version":         e.version,
		"timestamp":       time.Now().UTC().Format(time.RFC3339),
		"level":           level,
		"message":         message,
	}
}
