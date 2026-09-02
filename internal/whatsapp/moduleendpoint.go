package whatsapp

import (
	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	fabricpb "github.com/aceteam-ai/fabric-protocol/gen/go/aceteam/fabric/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// BridgeModuleInputs are the facts BuildBridgeModule needs to construct the
// synthetic WhatsApp-bridge ActualModule row (citadel-cli#624 Phase A). The
// bridge is deployed outside the module system (no modules.lock entry), so
// unlike every other reported module its facts are gathered from the
// provisioned-service registry, the bridge env file, the gateway's persisted
// facts, and a live container check -- rather than from a lockfile entry.
//
// Kept as plain data (not injected functions) so BuildBridgeModule is a pure,
// dependency-free mapping any caller can unit test without docker, the mesh,
// or a filesystem. Callers (the node-state reporters, via their injected
// BridgeEndpointsProvider) own gathering these values fresh on every call, so
// the two reporters that each hold a reference to the SAME provider report
// byte-identical bridge facts rather than two independently-computed
// snapshots that could disagree (the "two-reporter flap", citadel#624 design
// comment).
type BridgeModuleInputs struct {
	// Deployed is whether the bridge compose has been materialized on this
	// node at all (whatsapp.IsDeployed). false means there is nothing to
	// report and BuildBridgeModule returns nil, UNLESS Registered is true (a
	// registry entry survives a manual compose-file removal until the next
	// backfill sweep; still worth reporting as ABSENT+its stale endpoint fact
	// so drift is visible rather than silently dropped).
	Deployed bool
	// Registered is whether the bridge has a provisioned-service registry
	// entry (i.e. is exposed, or was exposed, on the gateway).
	Registered bool
	// Prefix is the registry entry's gateway route prefix. Only meaningful
	// when Registered.
	Prefix string
	// BridgePort is the registry entry's Port -- the bridge's own loopback
	// host port. 0 means "declared but not deployed" (see
	// provisionedservice.Entry.Port's doc): the endpoint is skipped entirely
	// rather than reported with a misleading port.
	BridgePort int
	// GatewayPort/GatewayUseTLS describe the LIVE gateway, not the bridge's
	// raw loopback port -- the endpoint's reachable port is the gateway's,
	// since the backend reaches the bridge through the gateway route, never
	// the raw port directly (citadel-cli#447). GatewayPort <= 0 means the
	// gateway's facts are unknown (e.g. no `citadel work` has ever run with
	// the gateway on this node); the endpoint is skipped in that case too --
	// a port-less endpoint would misrepresent an address nothing serves.
	GatewayPort   int
	GatewayUseTLS bool
	// ContainerRunning is a live, bridge-specific health signal (`docker
	// compose -p <ProjectName> ps`, scoped to the bridge service), NOT the
	// generic citadel-<name> dockerInspector convention -- the bridge's
	// container is named `<project>-bridge-N` (citadel-cli#436), which the
	// generic convention never matches, so it would otherwise report STOPPED
	// forever.
	ContainerRunning bool
	// AdminKeyFingerprint is whatsapp.AdminKeyFingerprint(env["ADMIN_API_KEY"]),
	// "" when the env file is missing or carries no admin key.
	AdminKeyFingerprint string
}

// BuildBridgeModule maps BridgeModuleInputs onto the synthetic ActualModule
// row citadel-cli#624 Phase A reports for the WhatsApp bridge -- a module with
// no modules.lock entry of its own, so it is keyed by Source = ServiceName
// (ActualModule carries no separate name field to key by instead).
//
// This function never touches modules.lock, and this package imports nothing
// from internal/catalog -- structurally, the row it returns cannot become
// reconcile/uninstall-eligible (internal/reconcile.Reconcile drives its
// converge/uninstall set off the lockfile alone; this row never enters it).
//
// Returns nil when there is nothing to report (bridge never deployed and
// never registered on the gateway).
func BuildBridgeModule(in BridgeModuleInputs) *fabricpb.ActualModule {
	if !in.Deployed && !in.Registered {
		return nil
	}

	m := &fabricpb.ActualModule{
		Source:    ServiceName,
		UpdatedAt: timestamppb.Now(),
	}

	switch {
	case !in.Deployed:
		// Registered but no compose materialized (a stale registry entry):
		// report ABSENT rather than fabricating a running/stopped verdict.
		m.Status = fabricpb.ModuleStatus_MODULE_STATUS_ABSENT
		m.Health = fabricpb.ModuleHealth_MODULE_HEALTH_UNSPECIFIED
	case in.ContainerRunning:
		m.Status = fabricpb.ModuleStatus_MODULE_STATUS_RUNNING
		m.Health = fabricpb.ModuleHealth_MODULE_HEALTH_HEALTHY
	default:
		// Deployed but not currently running: STOPPED, not an error -- mirrors
		// nodestate.dockerInspector's convention for a cleanly-stopped module.
		m.Status = fabricpb.ModuleStatus_MODULE_STATUS_STOPPED
		m.Health = fabricpb.ModuleHealth_MODULE_HEALTH_UNSPECIFIED
	}

	if in.Registered && in.BridgePort > 0 && in.GatewayPort > 0 {
		scheme := "https"
		if !in.GatewayUseTLS {
			scheme = "http"
		}
		m.Endpoints = append(m.Endpoints, &fabricpb.ModuleEndpoint{
			Name:                BridgeService,
			Kind:                "rest",
			Scheme:              scheme,
			Port:                uint32(in.GatewayPort),
			Path:                gateway.ModuleRoutePath(in.Prefix),
			Health:              m.Health,
			HealthPath:          HealthPath,
			AdminKeyFingerprint: in.AdminKeyFingerprint,
		})
	}

	return m
}
