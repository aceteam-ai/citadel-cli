package nodestate

import (
	"context"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"google.golang.org/protobuf/proto"
)

// DefaultInterval is the default node-state reporting period.
const DefaultInterval = 60 * time.Second

// emitTimeout bounds a single report (build + serialize + POST) so a slow or
// hung control plane can never wedge the reporter goroutine.
const emitTimeout = 30 * time.Second

// StatePoster ships a serialized ActualState to the control plane. The live
// implementation POSTs binary protobuf to the device-authed binary endpoint;
// tests inject a fake to assert path/content-type/body without a real backend.
type StatePoster interface {
	// PostNodeState POSTs the binary-protobuf-encoded ActualState. Implementations
	// set Content-Type: application/octet-stream and authenticate with the node's
	// device identity (the required device_state:write scope is enforced
	// server-side; the client cannot grant it).
	PostNodeState(ctx context.Context, body []byte) error
}

// Emitter periodically builds the node's ActualState and posts it upstream. It
// is fire-and-forget and crash-safe: a single failed report is dropped and the
// loop keeps ticking; a panic in one cycle never crashes the worker.
type Emitter struct {
	poster    StatePoster
	inspector ModuleInspector
	configDir string // where telemetry.yaml lives; the opt-out flag is re-read here per cycle
	nodeID    string // Headscale hostname (server auth/identity key)
	version   string // citadel-cli version (agent_version)
	interval  time.Duration
}

// Config wires up an Emitter.
type Config struct {
	// Poster ships the serialized report upstream. Required; a nil Poster makes
	// New return nil (reporting disabled).
	Poster StatePoster
	// Inspector observes per-module run-state. May be nil (e.g. no docker), in
	// which case modules report UNSPECIFIED status/health.
	Inspector ModuleInspector
	// ConfigDir is where telemetry.yaml lives; the opt-out flag is re-read from
	// here each cycle so a runtime toggle takes effect without a restart.
	ConfigDir string
	// NodeID is the Headscale hostname.
	NodeID string
	// Version is the citadel-cli version, reported as agent_version.
	Version string
	// Interval is the reporting period. Zero defaults to DefaultInterval.
	Interval time.Duration
}

// New builds an Emitter. It returns nil when reporting cannot run (no poster or
// no node identity), so callers can wire it unconditionally and treat nil as a
// silent no-op.
func New(cfg Config) *Emitter {
	if cfg.Poster == nil || cfg.NodeID == "" {
		return nil
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Emitter{
		poster:    cfg.Poster,
		inspector: cfg.Inspector,
		configDir: cfg.ConfigDir,
		nodeID:    cfg.NodeID,
		version:   cfg.Version,
		interval:  interval,
	}
}

// Run drives the reporting loop until ctx is cancelled. It is safe to call in a
// goroutine. A nil receiver is a no-op so a disabled emitter needs no guard at
// the call site. The first report fires immediately, then every Interval.
func (e *Emitter) Run(ctx context.Context) {
	if e == nil {
		return
	}
	e.reportOnce(ctx)

	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.reportOnce(ctx)
		}
	}
}

// reportOnce performs one report cycle with full crash isolation: it gates on
// the opt-out flag, builds + serializes the report, and posts it — recovering
// from any panic and bounding the whole cycle with a timeout. Failures are
// intentionally dropped; node-state reporting is best-effort.
func (e *Emitter) reportOnce(parent context.Context) {
	defer func() {
		// Reporting must never crash the worker; swallow any panic (cf. #291).
		_ = recover()
	}()

	// Re-read the opt-out flag per cycle so the settings toggle takes effect at
	// runtime without a restart — the same gate as activity telemetry.
	if !config.LoadTelemetry(e.configDir).AnonTelemetryEnabled {
		return
	}

	ctx, cancel := context.WithTimeout(parent, emitTimeout)
	defer cancel()

	state := BuildActualState(ctx, e.inspector, e.nodeID, e.version)
	body, err := proto.Marshal(state)
	if err != nil {
		return
	}
	_ = e.poster.PostNodeState(ctx, body)
}
