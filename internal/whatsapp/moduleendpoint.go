package whatsapp

import (
	"strings"

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

// AttachBridgeModule merges the bridge's ActualModule row (m, from
// BuildBridgeModule) into state.Modules WITHOUT creating a duplicate
// (citadel#624 sub-collision 4). It is a DECORATOR, and it is the ONE place
// both node-state reporters (nodestate.Emitter and reconcile.ProtoProvider)
// attach the bridge facts, so the two agree.
//
// Once the bridge is a first-class lockfile module (citadel#624 Part 1), both
// reporters already emit a REAL row for it -- keyed by Source == ServiceName --
// enumerated from the lockfile. Appending m unchanged (Phase A's behavior) would
// then double-report the same source; ingest is upsert-by-(node_id, source), so
// the two rows would flap. So:
//
//   - When a real row with the SAME Source already exists, attach m's Endpoints
//     (the admin-key fingerprint + gateway route facts Phase A carries) to THAT
//     row, and adopt m's Status/Health -- the bridge-specific compose-project
//     probe BuildBridgeModule used is authoritative for this module, unlike the
//     generic citadel-<name> inspector that produced the real row's health
//     (which never matches the bridge's <project>-bridge-N container, #436). A
//     genuine converge ERROR on the real row is preserved (a louder, distinct
//     signal), never overwritten by a probe reading.
//   - When NO real row exists (an old node / bespoke deploy with no lockfile
//     entry), m is appended as the synthetic row, exactly as Phase A did.
//
// A nil state or nil m is a no-op, so callers need no guard of their own.
func AttachBridgeModule(state *fabricpb.ActualState, m *fabricpb.ActualModule) {
	if state == nil || m == nil {
		return
	}
	for _, existing := range state.Modules {
		if existing == nil || !sourceNamesBridge(existing.Source) {
			continue
		}
		existing.Endpoints = m.Endpoints
		if existing.Health != fabricpb.ModuleHealth_MODULE_HEALTH_ERROR {
			existing.Status = m.Status
			existing.Health = m.Health
		}
		return
	}
	state.Modules = append(state.Modules, m)
}

// sourceNamesBridge reports whether an ActualModule Source refers to the WhatsApp
// bridge -- whether recorded by its sanctioned catalog NAME ("whatsapp-bridge" ==
// ServiceName, the D2 install path) OR a git-SOURCE form
// ("owner/whatsapp-bridge[@ref]", ".../whatsapp-bridge.git"). Both reduce to the
// same canonical module name, so the decorator keys on that rather than raw
// string equality (citadel#624 FIX D): otherwise a git-source-installed bridge
// (lockfile Source "sunapi386/whatsapp-bridge") would never match the synthetic
// row (Source == ServiceName) and the bridge would be PERMANENTLY double-reported
// under an upsert-by-source ingest. It mirrors reconcile.NameFromSource's
// canonicalization for the forms that matter; whatsapp cannot import reconcile
// (reconcile imports whatsapp), so it is duplicated here and pinned by
// TestSourceNamesBridge.
func sourceNamesBridge(source string) bool {
	return canonicalModuleName(source) == ServiceName
}

// canonicalModuleName reduces a module source string (catalog name |
// owner/repo[@ref] | git URL) to its canonical module name -- the repo/basename
// segment, ref and ".git" stripped. A faithful mirror of reconcile.NameFromSource
// for the source shapes the bridge can be installed from.
func canonicalModuleName(source string) string {
	s := strings.TrimSpace(source)
	if s == "" {
		return ""
	}
	// Strip an "@ref" suffix, but NOT the "@" of an scp-style git URL
	// ("git@github.com:owner/repo.git"), where "@" precedes the host (a ref never
	// contains "/" or ":").
	if at := strings.LastIndex(s, "@"); at > 0 {
		if tail := s[at+1:]; tail != "" && !strings.ContainsAny(tail, "/:") {
			s = s[:at]
		}
	}
	if !strings.ContainsAny(s, "/:") {
		return s // bare catalog name
	}
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSuffix(s, ".git")
}
