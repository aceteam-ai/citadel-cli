// Package nodestate builds and emits the node's ActualState report to the
// AceTeam control plane (citadel#353, report-only v1).
//
// "Report-only" means this package answers one question — "what is this node
// actually running?" — and ships the answer upstream. It does NOT pull a
// DesiredState or converge toward it; that is v2 (the existing
// internal/reconcile package holds the deferred converge engine, which v2 will
// migrate onto this same protobuf contract).
//
// The wire contract is protobuf (the published
// github.com/aceteam-ai/fabric-protocol Go module, imported here under the
// fabricpb alias). The report is serialized binary and POSTed to a
// new device-authed binary endpoint; a control-plane worker XADDs it to the
// node:state:stream Redis stream and decodes it into relational columns.
//
// Design constraints (mirrors the #4139 activity telemetry path):
//   - node_id is the Headscale hostname; the server re-derives org and ignores
//     any node-reported org claim (same spoofing defense).
//   - Per-module failure isolation: one module that fails to inspect reports
//     MODULE_HEALTH_ERROR + an error string, and never aborts the whole report.
//   - Fire-and-forget + crash-safe: emission runs off-thread with a timeout and
//     a panic recover, and never crashes the worker (cf. #291).
//   - Opt-out: gated by the same anon_telemetry_enabled flag as activity
//     telemetry, re-read per emit so a runtime toggle takes effect.
package nodestate

import (
	"context"
	"sort"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/protocol"
	fabricpb "github.com/aceteam-ai/fabric-protocol/gen/go/aceteam/fabric/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// loadLockfile reads the installed-module lockfile. It is a package var so
// tests can inject a fixture without redirecting the on-disk config dir; the
// production value reads modules.lock via the catalog package.
var loadLockfile = catalog.LoadLockfile

// Observation is the live run-state of one installed module, as observed by a
// ModuleInspector (e.g. a docker inspect). It is deliberately decoupled from
// docker so the builder is unit-testable with a fake.
type Observation struct {
	// Status is the observed module status (running / stopped / absent).
	Status fabricpb.ModuleStatus
	// Health is the observed module health.
	Health fabricpb.ModuleHealth
}

// ModuleInspector observes the live run-state of an installed module by name.
//
// CONTRACT (per-module failure isolation): a non-nil error for ONE module must
// not abort the whole report. The builder catches it, marks that module
// MODULE_HEALTH_ERROR with the error string, and continues with the rest. A
// real implementation (docker) returns an error only when it genuinely cannot
// determine state; "container not found" should be reported as a STOPPED/ABSENT
// observation, not an error, so a cleanly-stopped module is not noise.
type ModuleInspector interface {
	Inspect(ctx context.Context, moduleName string) (Observation, error)
}

// BridgeEndpointsProvider builds the synthetic ActualModule row (module +
// endpoints) for a node-hosted module that carries no modules.lock entry of
// its own -- citadel#624 Phase A's only instance is the WhatsApp bridge. It is
// defined identically (same method shape) in internal/reconcile so that
// package's ProtoProvider can accept the SAME concrete value via Go's
// structural typing without either package importing the other (mirrors the
// internal/status / internal/worker split for the identical reason -- see
// that package's SwapActivity note). Both node-state reporters (this
// package's Emitter and reconcile.ProtoProvider) hold a reference to the SAME
// provider instance, so they report byte-identical bridge facts rather than
// two independently-computed snapshots that could disagree -- the
// "two-reporter flap" the citadel#624 design review called out as
// must-resolve.
//
// A nil return from BridgeModule means nothing to report yet (e.g. the
// module has never been deployed on this node).
type BridgeEndpointsProvider interface {
	BridgeModule(ctx context.Context) *fabricpb.ActualModule
}

// BuildActualState constructs the node's ActualState from the installed-module
// set (the modules.lock lockfile) plus a live per-module inspection.
//
// nodeID is the Headscale hostname (the server's auth/identity key).
// agentVersion is the citadel-cli version. The report's applied_revision is ""
// in v1 (there is no DesiredState to echo yet), and config_ref is "" per module
// (no desired config to hash against yet).
//
// It never returns an error: a failure to read the lockfile yields a report
// with no modules (the node is still reporting "I am alive, running nothing I
// can see"), and a per-module inspect failure is isolated into that module's
// ERROR health. This keeps emission unconditional and crash-free.
func BuildActualState(ctx context.Context, insp ModuleInspector, nodeID, agentVersion string) *fabricpb.ActualState {
	state := newEnvelope(nodeID, agentVersion)

	lf, err := loadLockfile()
	if err != nil || lf == nil {
		// No readable lockfile => report zero modules. The envelope still goes
		// out so the control plane sees the node is reporting.
		return state
	}

	entries := make([]catalog.LockEntry, len(lf.Modules))
	copy(entries, lf.Modules)
	// Deterministic module order makes the report stable and tests simple.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	now := timestamppb.Now()
	for _, e := range entries {
		state.Modules = append(state.Modules, buildModule(ctx, insp, e, now))
	}
	return state
}

// newEnvelope builds an empty ActualState envelope (no modules) with the
// header every report shares. Factored out so the telemetry-gate-exempt
// bridge-only path (Emitter.reportOnce, when anon_telemetry_enabled is false)
// can build the same header without pulling in the lockfile-driven module
// enumeration BuildActualState performs.
func newEnvelope(nodeID, agentVersion string) *fabricpb.ActualState {
	return &fabricpb.ActualState{
		ProtocolVersion: uint32(protocol.FabricProtocolVersion),
		NodeId:          nodeID,
		AppliedRevision: "", // no desired-state in v1
		AgentVersion:    agentVersion,
		ReportedAt:      timestamppb.Now(),
	}
}

// AppendBridgeModule appends p's synthetic bridge module row (if any) onto
// state.Modules. Exported so both this package's Emitter (for the
// telemetry-gate-exempt path, where the row must be attached to an envelope
// BuildActualState was not used to build) and the WHATSAPP_PROVISION-adjacent
// wiring in cmd can attach the identical row this contract describes, with
// the nil-provider / nil-module handling centralized in one place.
//
// A nil state or nil provider is a no-op, so callers need no guard of their
// own.
func AppendBridgeModule(ctx context.Context, state *fabricpb.ActualState, p BridgeEndpointsProvider) {
	if state == nil || p == nil {
		return
	}
	if m := p.BridgeModule(ctx); m != nil {
		state.Modules = append(state.Modules, m)
	}
}

// buildModule maps one lockfile entry plus a live inspection into an
// ActualModule. The mapping resolves the contract mismatches between the
// lockfile shape and the proto shape:
//
//   - installed_version <- ResolvedRef, else Ref, else Commit.
//   - image_digest      <- the first image's digest (proto carries one string;
//     the lockfile carries a slice). "" when no digest is resolvable.
//   - config_ref        <- "" (no desired config to hash against in v1).
//   - status / health   <- from the live inspection. A per-module inspect error
//     is isolated here as MODULE_HEALTH_ERROR + the error string; status is left
//     UNSPECIFIED and the rest of the report is unaffected.
func buildModule(ctx context.Context, insp ModuleInspector, e catalog.LockEntry, now *timestamppb.Timestamp) *fabricpb.ActualModule {
	m := &fabricpb.ActualModule{
		Source:           e.Source,
		InstalledVersion: installedVersion(e),
		ImageDigest:      firstDigest(e.Images),
		ConfigRef:        "", // no desired config to compare against in v1
		UpdatedAt:        now,
	}

	if insp == nil {
		m.Status = fabricpb.ModuleStatus_MODULE_STATUS_UNSPECIFIED
		m.Health = fabricpb.ModuleHealth_MODULE_HEALTH_UNSPECIFIED
		return m
	}

	obs, err := insp.Inspect(ctx, e.Name)
	if err != nil {
		// Per-module failure isolation: this module reports ERROR; the caller
		// keeps building the rest of the report.
		m.Status = fabricpb.ModuleStatus_MODULE_STATUS_UNSPECIFIED
		m.Health = fabricpb.ModuleHealth_MODULE_HEALTH_ERROR
		m.Error = err.Error()
		return m
	}

	m.Status = obs.Status
	m.Health = obs.Health
	return m
}

// installedVersion picks the most specific resolved identifier the lockfile has
// for an installed module: the concrete tag a constraint resolved to, else the
// requested ref, else the resolved git commit.
func installedVersion(e catalog.LockEntry) string {
	if e.ResolvedRef != "" {
		return e.ResolvedRef
	}
	if e.Ref != "" {
		return e.Ref
	}
	return e.Commit
}

// firstDigest returns the digest of the first image with one, or "". The proto
// carries a single image_digest while a module may deploy several images; the
// first image's digest is the representative running-image digest.
func firstDigest(images []catalog.LockImage) string {
	for _, img := range images {
		if img.Digest != "" {
			return img.Digest
		}
	}
	return ""
}
