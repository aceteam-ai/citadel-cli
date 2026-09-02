package nodestate

import (
	"context"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	fabricpb "github.com/aceteam-ai/fabric-protocol/gen/go/aceteam/fabric/v1"
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
	poster          StatePoster
	inspector       ModuleInspector
	bridgeEndpoints BridgeEndpointsProvider
	configDir       string // where telemetry.yaml lives; the opt-out flag is re-read here per cycle
	nodeID          string // Headscale hostname (server auth/identity key)
	version         string // citadel-cli version (agent_version)
	interval        time.Duration
}

// Config wires up an Emitter.
type Config struct {
	// Poster ships the serialized report upstream. Required; a nil Poster makes
	// New return nil (reporting disabled).
	Poster StatePoster
	// Inspector observes per-module run-state. May be nil (e.g. no docker), in
	// which case modules report UNSPECIFIED status/health.
	Inspector ModuleInspector
	// BridgeEndpoints builds the synthetic bridge module row (citadel#624 Phase
	// A). May be nil (no node-hosted module with its own network endpoints to
	// report), in which case no synthetic row is ever appended. When set, its
	// row is exempt from the AnonTelemetryEnabled gate below -- see
	// reportOnce's doc comment.
	BridgeEndpoints BridgeEndpointsProvider
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
		poster:          cfg.Poster,
		inspector:       cfg.Inspector,
		bridgeEndpoints: cfg.BridgeEndpoints,
		configDir:       cfg.ConfigDir,
		nodeID:          cfg.NodeID,
		version:         cfg.Version,
		interval:        interval,
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

// reportOnce performs one report cycle with full crash isolation: it builds +
// serializes the report and posts it — recovering from any panic and bounding
// the whole cycle with a timeout. Failures are intentionally dropped;
// node-state reporting is best-effort.
//
// The AnonTelemetryEnabled opt-out gates the LOCKFILE-DRIVEN module telemetry
// (BuildActualState) only — re-read per cycle so a runtime toggle takes
// effect without a restart, same as activity telemetry. It deliberately does
// NOT gate BridgeEndpoints: a node-hosted module's network endpoints are
// OPERATIONAL facts the platform needs to reach it, not telemetry, so riding
// the opt-out would make an opted-out node's bridge silently unreachable/stale
// upstream (citadel#624 design review, point 2). When telemetry is off and
// there is nothing operational to report either, the original opt-out
// behavior is preserved exactly: no report is posted at all.
func (e *Emitter) reportOnce(parent context.Context) {
	defer func() {
		// Reporting must never crash the worker; swallow any panic (cf. #291).
		_ = recover()
	}()

	ctx, cancel := context.WithTimeout(parent, emitTimeout)
	defer cancel()

	telemetryOn := config.LoadTelemetry(e.configDir).AnonTelemetryEnabled

	var state *fabricpb.ActualState
	if telemetryOn {
		state = BuildActualState(ctx, e.inspector, e.nodeID, e.version)
	} else {
		state = newEnvelope(e.nodeID, e.version)
	}
	AppendBridgeModule(ctx, state, e.bridgeEndpoints)

	if !telemetryOn && len(state.GetModules()) == 0 {
		return
	}

	body, err := proto.Marshal(state)
	if err != nil {
		return
	}
	_ = e.poster.PostNodeState(ctx, body)
}
