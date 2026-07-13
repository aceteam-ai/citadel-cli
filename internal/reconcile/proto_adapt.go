package reconcile

import (
	"strings"

	fabricpb "github.com/aceteam-ai/fabric-protocol/gen/go/aceteam/fabric/v1"
)

// adaptDesiredState converts a control-plane protobuf DesiredState (the
// aceteam#4273 wire contract) into the engine's internal DesiredState.
//
// It carries the revision through for the handshake and maps each proto
// DesiredModule to an internal ModuleAssignment, OMITTING modules that must not
// appear in the authoritative desired set:
//
//   - MODULE_STATUS_ABSENT      -> omitted, so the engine (which treats desired
//     as authoritative for the whole node) uninstalls it if installed. This is
//     the same idiom the MODULE_SET handler uses for "absent".
//   - MODULE_STATUS_UNSPECIFIED -> rejected/omitted: a module with no target
//     state is meaningless and must never drive a converge.
//   - empty source              -> rejected/omitted: without a source the engine
//     cannot install or key the module.
//
// nil pb yields a zero DesiredState (no revision, no modules).
func adaptDesiredState(pb *fabricpb.DesiredState) DesiredState {
	if pb == nil {
		return DesiredState{}
	}
	ds := DesiredState{Revision: pb.GetRevision()}
	for _, m := range pb.GetModules() {
		if am, ok := adaptDesiredModule(m); ok {
			ds.Modules = append(ds.Modules, am)
		}
	}
	return ds
}

// adaptDesiredModule maps one proto DesiredModule to a ModuleAssignment. The
// bool is false when the module must be OMITTED from the desired set (ABSENT,
// UNSPECIFIED, unknown status, or empty source) — see adaptDesiredState.
//
// The proto has no explicit name field; the reconciliation key is derived from
// Source via ModuleAssignment.Key()/NameFromSource. Source is passed through
// UNCHANGED to preserve the requested-ref equality contract (types.go:141-155):
// the control plane MUST assign Source in the same REQUESTED-ref form the node
// reports in ActualState (e.g. "owner/repo@^1.2", not a resolved tag), or the
// engine sees drift and re-updates every pass.
func adaptDesiredModule(m *fabricpb.DesiredModule) (ModuleAssignment, bool) {
	if m == nil {
		return ModuleAssignment{}, false
	}
	if strings.TrimSpace(m.GetSource()) == "" {
		return ModuleAssignment{}, false
	}

	var status DesiredStatus
	switch m.GetDesiredStatus() {
	case fabricpb.ModuleStatus_MODULE_STATUS_RUNNING:
		status = StatusRunning
	case fabricpb.ModuleStatus_MODULE_STATUS_STOPPED:
		status = StatusStopped
	case fabricpb.ModuleStatus_MODULE_STATUS_ABSENT:
		// Absent is realized by OMITTING the module: the authoritative engine
		// uninstalls anything installed but not in desired.
		return ModuleAssignment{}, false
	default:
		// UNSPECIFIED or any unknown enum value: reject.
		return ModuleAssignment{}, false
	}

	return ModuleAssignment{
		Source:          m.GetSource(),
		Config:          m.GetConfig(),
		DesiredStatus:   status,
		AllowPrivileged: m.GetAllowPrivileged(),
	}, true
}

// healthToProto maps the internal observed ModuleHealth onto the proto
// (status, health) pair for the actual-state report. The internal model folds
// run-state and health into one enum, so the mapping is deliberately lossy but
// deterministic:
//
//	HealthRunning -> (RUNNING, HEALTHY)
//	HealthStopped -> (STOPPED, UNSPECIFIED)   // stopped-but-fine, no health signal
//	HealthError   -> (UNSPECIFIED, ERROR)     // converge/install failed
//	HealthUnknown / other -> (UNSPECIFIED, UNSPECIFIED)
func healthToProto(h ModuleHealth) (fabricpb.ModuleStatus, fabricpb.ModuleHealth) {
	switch h {
	case HealthRunning:
		return fabricpb.ModuleStatus_MODULE_STATUS_RUNNING, fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY
	case HealthStopped:
		return fabricpb.ModuleStatus_MODULE_STATUS_STOPPED, fabricpb.ModuleHealth_MODULE_HEALTH_UNSPECIFIED
	case HealthError:
		return fabricpb.ModuleStatus_MODULE_STATUS_UNSPECIFIED, fabricpb.ModuleHealth_MODULE_HEALTH_ERROR
	default:
		return fabricpb.ModuleStatus_MODULE_STATUS_UNSPECIFIED, fabricpb.ModuleHealth_MODULE_HEALTH_UNSPECIFIED
	}
}
