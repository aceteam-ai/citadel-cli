package status

import (
	"strings"
	"sync"
	"time"
)

// request_recorder.go closes the last_request_at gap tracked in citadel #691:
// idleCapableEngines (engines.go) and idleEngineType (collector.go) only cover
// engines that expose a scrapeable Prometheus request counter -- today, just
// vLLM's own metrics dialect. The #433 network-activity tracker (footprint.go)
// widens that with a NetIO-based heuristic, but it is still indirect: it
// depends on the container runtime reporting a real, changing NetIO reading
// for that container. What we can assert from the code (not verified live on
// the #691 node): RecordActivityCounter only ever refreshes last_request_at on
// a CHANGE from a prior sample (footprint.go), so the first scrape after
// process start never proves a request on its own, and a container whose
// stats read never yields a usable NetIO cell (fp.HasNet=false -- host
// networking, an older runtime, a transient read miss) never feeds the
// tracker at all. Either is consistent with the "ollama/bonsai/vllm report
// never" symptom the issue describes; this fix does not depend on which one
// it actually was, because it does not scrape at all.
//
// The fix here is direct rather than inferred: the two places THIS node
// dispatches a request to a local engine on a caller's behalf -- the gateway's
// model->engine chat router (internal/gateway/chat_route.go) and the worker's
// llm_inference handler (internal/worker/llm_inference.go) -- record the
// dispatch against the engine name as it happens. No scraping, no metrics
// dialect, no cooperation required from the engine.
//
// Coverage limits, stated honestly:
//   - Direct-to-port traffic: this only sees requests that flow THROUGH this
//     node's own routing paths. A request that reaches an engine's host port
//     directly -- a peer dialing the port itself, a human curling
//     localhost:<port>, or any other caller that bypasses the gateway/worker --
//     is invisible here, exactly as it always was to the vLLM Prometheus scrape
//     and the #433 NetIO heuristic (both of which DO see direct-to-port
//     traffic, just less precisely for non-vLLM engines). An engine that has
//     received neither a scraped signal nor a node-routed request still,
//     correctly, reports last_request_at as "never" -- this package never
//     fabricates a timestamp.
//   - Process-local, not persisted: the log lives only in the memory of the
//     process that recorded the request. Two consequences worth naming rather
//     than discovering later (the #692/#733 pattern this file's package
//     comment warns about): (1) a fresh `citadel status` or `citadel services`
//     invocation is a NEW process with an empty log, so it still reports
//     "never" for an engine whose only traffic went through a long-lived
//     `citadel work`'s gateway/worker in a DIFFERENT process, even though that
//     process's own heartbeat shows a real timestamp; (2) a restart of the
//     recording process (crash, `citadel update`, a manual restart) resets the
//     log to empty, same as the existing IdleTracker/FootprintIdleTracker
//     instances it sits alongside. Cross-process/persisted sharing was not
//     asked for here and is not implemented.

// requestLog is a process-wide, in-memory record of the last time this node
// routed a request to a given serving engine. Safe for concurrent use.
type requestLog struct {
	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}

// newRequestLog returns an empty requestLog using the real clock. Tests that
// need a deterministic clock construct one directly and override now.
func newRequestLog() *requestLog {
	return &requestLog{last: make(map[string]time.Time), now: time.Now}
}

// record stamps engine as having just received a node-routed request. A blank
// engine name (after trimming) is a no-op: callers pass a resolved
// backend/engine name (vllm/ollama/bonsai/llamacpp/unlimited-ocr), never raw
// user input.
func (l *requestLog) record(engine string) {
	key := normalizeEngineKey(engine)
	if key == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.last[key] = l.now()
}

// lastRequestAt returns the last recorded request time for engine and whether
// one has ever been recorded.
func (l *requestLog) lastRequestAt(engine string) (time.Time, bool) {
	key := normalizeEngineKey(engine)
	if key == "" {
		return time.Time{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.last[key]
	return t, ok
}

// idleState derives an IdleState purely from the request log, honoring the
// given idle threshold. ok=false means no node-routed request has ever been
// recorded for engine: callers MUST treat that as "no local signal" (fall
// through to the next fallback, or report "never"), never as "definitely
// idle" or "just requested".
func (l *requestLog) idleState(engine string, threshold time.Duration) (IdleState, bool) {
	t, ok := l.lastRequestAt(engine)
	if !ok {
		return IdleState{}, false
	}
	idleFor := l.now().Sub(t)
	if idleFor < 0 {
		idleFor = 0
	}
	last := t
	return IdleState{
		Idle:          idleFor >= threshold,
		IdleSeconds:   int64(idleFor.Seconds()),
		LastRequestAt: &last,
	}, true
}

func normalizeEngineKey(engine string) string {
	return strings.ToLower(strings.TrimSpace(engine))
}

// nodeRequestLog is the process-wide default request log. Every Collector
// created via NewCollector shares this instance (the reqLog field) so a
// request recorded by this process's gateway or worker is visible to this
// same process's heartbeat collector. It is intentionally NOT shared across
// processes or persisted: a restarted citadel starts with an empty log, same
// as the existing IdleTracker instances.
var nodeRequestLog = newRequestLog()

// RecordEngineRequest records that this node just routed a request to the
// named serving engine (e.g. "ollama", "bonsai", "vllm", "llamacpp",
// "unlimited-ocr"). Called from:
//   - internal/gateway.Server.SetRequestRecorder (the chat router's dispatch
//     point, wired in cmd/gateway_chat.go)
//   - internal/worker.LLMInferenceHandler (the worker's llm_inference
//     dispatch point, defaulted in its constructor)
//
// See the package-level doc comment above for the direct-to-port coverage
// limit this cannot see.
func RecordEngineRequest(engine string) {
	nodeRequestLog.record(engine)
}
