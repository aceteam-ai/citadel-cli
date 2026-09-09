// internal/status/model_license.go
package status

import "sync"

// modelLicenseLog is a process-wide, in-memory record of the last known
// model_license string an engine's own /info endpoint reported for the model
// it is currently serving (citadel-cli#1007, the OmniVoice onboarding:
// OmniVoice's checkpoint is CC-BY-NC, unlike kokoro's Apache-2.0 Kokoro-82M,
// so the heartbeat needs an honest per-engine signal rather than assuming
// every TTS backend is uniformly safe to use commercially).
//
// Mirrors request_recorder.go's requestLog shape and its stated tradeoffs:
// process-local, unpersisted (a restart resets it to empty), and populated
// only by callers that actually dispatch to the engine -- there is no scrape
// path for this today, only internal/jobs.SynthesizeSpeechHandler's
// best-effort GET /info after a successful synthesis.
type modelLicenseLog struct {
	mu      sync.Mutex
	license map[string]string
}

func newModelLicenseLog() *modelLicenseLog {
	return &modelLicenseLog{license: make(map[string]string)}
}

// record stores license against engine. A blank engine (after normalizing) or
// a blank license is a no-op -- callers pass a resolved backend name and a
// non-empty license string, never raw/unvalidated input.
func (l *modelLicenseLog) record(engine, license string) {
	key := normalizeEngineKey(engine)
	if key == "" || license == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.license[key] = license
}

func (l *modelLicenseLog) get(engine string) (string, bool) {
	key := normalizeEngineKey(engine)
	if key == "" {
		return "", false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.license[key]
	return v, ok
}

// nodeModelLicenseLog is the process-wide default model-license log, mirroring
// nodeRequestLog (request_recorder.go): every Collector created via
// NewCollector reads from this SAME instance, so a license recorded by this
// process's SYNTHESIZE_SPEECH handler is visible to this same process's
// heartbeat collector.
var nodeModelLicenseLog = newModelLicenseLog()

// RecordModelLicense records the model_license string an engine's own /info
// endpoint reported. Called from internal/jobs.SynthesizeSpeechHandler after a
// successful synthesis + /info probe (citadel-cli#1007). A blank engine or
// license is a no-op.
func RecordModelLicense(engine, license string) {
	nodeModelLicenseLog.record(engine, license)
}

// ModelLicenseFor returns the last recorded model_license for engine, and
// whether one has ever been recorded this process. Exported for the
// collector's own attach pass (applyModelLicenseSignal, collector.go) and for
// internal/jobs to skip a redundant /info probe once a backend's license is
// already known.
func ModelLicenseFor(engine string) (string, bool) {
	return nodeModelLicenseLog.get(engine)
}
