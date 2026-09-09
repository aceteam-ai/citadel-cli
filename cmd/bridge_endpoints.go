// cmd/bridge_endpoints.go
//
// The concrete BridgeEndpointsProvider (citadel#624 Phase A): it gathers the
// WhatsApp bridge's live facts from the SAME sources already used elsewhere in
// this package (the provisioned-service registry, the bridge env file, the
// persisted gateway facts, and the bridge-specific container check) and maps
// them through whatsapp.BuildBridgeModule -- the pure, unit-tested function
// that owns the actual ActualModule/ModuleEndpoint shape.
//
// This type is constructed ONCE (newWhatsAppBridgeModuleProvider) and the SAME
// instance is wired into both node-state reporters in cmd/work.go
// (nodestate.Config.BridgeEndpoints and reconcile.ProtoProvider.BridgeEndpoints)
// -- see nodestate.BridgeEndpointsProvider's doc comment for why a shared
// instance (not two independently-constructed ones) is what actually closes
// the "two-reporter flap" the citadel#624 design review flagged as
// must-resolve.
package cmd

import (
	"context"
	"path/filepath"

	"github.com/aceteam-ai/citadel-cli/internal/whatsapp"
	fabricpb "github.com/aceteam-ai/fabric-protocol/gen/go/aceteam/fabric/v1"
)

// whatsAppBridgeModuleProvider satisfies BOTH nodestate.BridgeEndpointsProvider
// and reconcile.BridgeEndpointsProvider via Go's structural typing (the two
// interfaces share the identical BridgeModule(ctx) method shape by design --
// see either package's doc comment for why they are not unified into one
// shared interface type).
type whatsAppBridgeModuleProvider struct{}

// newWhatsAppBridgeModuleProvider constructs the shared provider instance.
func newWhatsAppBridgeModuleProvider() *whatsAppBridgeModuleProvider {
	return &whatsAppBridgeModuleProvider{}
}

// BridgeModule gathers the bridge's live facts and maps them through
// whatsapp.BuildBridgeModule. It is READ-ONLY by design: it resolves the
// node's services directory via findAndReadManifest (never
// findOrCreateManifest), so a background reporting cycle running on a node
// with no manifest yet simply reports nothing rather than provisioning a node
// config skeleton as a side effect of a heartbeat tick.
func (whatsAppBridgeModuleProvider) BridgeModule(_ context.Context) *fabricpb.ActualModule {
	_, configDir, err := findAndReadManifest()
	if err != nil {
		return nil // no node config yet: nothing to report
	}
	servicesDir := filepath.Join(configDir, "services")

	deployed := whatsapp.IsDeployed(servicesDir)

	registered := false
	bridgePort := 0
	if entries, err := provisionedRegistry().List(); err == nil {
		for _, e := range entries {
			if e.Prefix == whatsappGatewayPrefix {
				registered = true
				bridgePort = e.Port
				break
			}
		}
	}

	if !deployed && !registered {
		return nil
	}

	env, _ := whatsapp.LoadEnv(servicesDir)
	fingerprint := whatsapp.AdminKeyFingerprint(env["ADMIN_API_KEY"])

	containerRunning := false
	if deployed {
		// Bridge-specific health (citadel#624 design review, point 3): the
		// generic citadel-<name> dockerInspector convention no longer matches
		// this module's container naming (<project>-bridge-N, #436), so it
		// would report STOPPED forever. bridgeContainerRunning resolves the
		// container by compose project instead (cmd/whatsapp.go).
		containerRunning = bridgeContainerRunning(whatsapp.ProjectName(servicesDir))
	}

	facts := gatewayFactsForURL()

	return whatsapp.BuildBridgeModule(whatsapp.BridgeModuleInputs{
		Deployed:            deployed,
		Registered:          registered,
		Prefix:              whatsappGatewayPrefix,
		BridgePort:          bridgePort,
		GatewayPort:         facts.Port,
		GatewayUseTLS:       facts.UseTLS,
		ContainerRunning:    containerRunning,
		AdminKeyFingerprint: fingerprint,
	})
}
