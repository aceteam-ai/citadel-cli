// cmd/provisioned_gateway.go
//
// Wiring that makes a provisioned, dynamically-ported service (starting with the
// WhatsApp bridge) reachable by the AceTeam backend over the tsnet mesh
// (aceteam-ai/citadel-cli#447).
//
// The problem: only the status (:8080), terminal (:7860), and HTTPS gateway
// (:8443) servers bind a tsnet listener on the node VPN IP at startup. A
// provisioned bridge binds an auto-selected FREE host port that nothing on the
// tsnet stack listens on, so advertising http://<vpn-ip>:<bridge-port> hands the
// backend an unreachable address.
//
// The fix (gateway-route approach): the gateway already binds a tsnet listener
// on the node VPN IP. We register a stable route (gateway.WhatsAppRoutePrefix)
// up front with an EMPTY upstream; when the bridge is provisioned on some host
// port, ExposeGatewayRoute points that route at 127.0.0.1:<bridge-port>. The
// backend then reaches the bridge at <scheme>://<vpn-ip>:<gateway-port>/whatsapp
// and the gateway strips the prefix, forwarding to the bridge unchanged.
//
// The gateway instance lives in runWork's scope and is created only when the
// gateway is enabled, so it is shared with the WHATSAPP_PROVISION handler (and
// the local CLI, which shares whatsappProvisionDeps) through the package-level
// hook set by setProvisionedServiceGateway. When no gateway is running the hook
// is nil and Provision relies on VerifyReachable to fail loud rather than report
// a false-green.
package cmd

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/whatsapp"
)

// meshHTTPClient builds an HTTP client for the node's OWN reachability probe of
// its gateway route. The gateway serves a self-signed cert (see tlscert), so the
// node's local probe skips verification -- it is dialing its own loopback/mesh
// IP, not a third party, purely to confirm the route is wired. This does not
// change how the backend reaches the bridge.
func meshHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- self-signed gateway cert, local self-probe only
		},
	}
}

// DefaultGatewayPort is the gateway's default HTTPS port (matches the
// --gateway-port default in cmd/work.go). The mesh URL for a provisioned service
// is deterministic from this port + the node's mesh IP, so the CLI and TUI can
// advertise the reachable gateway-route URL even from a process that is not
// itself running the gateway (the gateway runs in a separate `citadel work`).
const DefaultGatewayPort = 8443

// provisionedGatewayRef holds the live gateway server (for in-process route
// registration) plus the facts needed to build the reachable mesh URL. It is set
// by setProvisionedServiceGateway when runWork starts the in-process gateway.
type provisionedGatewayRef struct {
	gw     *gateway.Server
	port   int  // the gateway's HTTPS/HTTP port (workGatewayPort)
	useTLS bool // false only when --gateway-no-tls
}

var (
	provisionedGatewayMu  sync.RWMutex
	provisionedGatewayCur *provisionedGatewayRef
)

// setProvisionedServiceGateway records the live gateway so provisioned-service
// deps can register routes and compute reachable mesh URLs. Called from runWork
// after the gateway is created. Passing gw==nil clears it.
func setProvisionedServiceGateway(gw *gateway.Server, port int, useTLS bool) {
	provisionedGatewayMu.Lock()
	defer provisionedGatewayMu.Unlock()
	if gw == nil {
		provisionedGatewayCur = nil
		return
	}
	provisionedGatewayCur = &provisionedGatewayRef{gw: gw, port: port, useTLS: useTLS}
}

// getProvisionedServiceGateway returns the current gateway ref, or nil.
func getProvisionedServiceGateway() *provisionedGatewayRef {
	provisionedGatewayMu.RLock()
	defer provisionedGatewayMu.RUnlock()
	return provisionedGatewayCur
}

// gatewayPortAndTLS returns the gateway port + TLS posture to use when building
// the mesh URL. It prefers the live in-process gateway (exact facts) and falls
// back to the defaults (8443, TLS on) so a separate CLI/TUI process advertises
// the same reachable URL the node's `citadel work` gateway serves.
func gatewayPortAndTLS() (port int, useTLS bool) {
	if ref := getProvisionedServiceGateway(); ref != nil {
		return ref.port, ref.useTLS
	}
	return DefaultGatewayPort, true
}

// gatewayRouteURL builds the mesh URL a service exposed under prefix is reachable
// at: <scheme>://<vpnIP>:<port><prefix>. Pure so it is unit-testable. Returns ""
// for an empty vpnIP (off-mesh).
func gatewayRouteURL(useTLS bool, vpnIP string, port int, prefix string) string {
	if vpnIP == "" {
		return ""
	}
	scheme := "https"
	if !useTLS {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s:%d%s", scheme, vpnIP, port, prefix)
}

// whatsappMeshAPIURL returns the reachable gateway-route mesh URL for the
// WhatsApp bridge, or "" when the node is off-mesh.
//
// It intentionally ignores the bridge host port: the backend reaches the bridge
// through the gateway route, not the raw port. The port argument is kept to
// satisfy ProvisionDeps.MeshAPIURL's signature (and it is what ExposeGatewayRoute
// wires the route to). The URL is deterministic from the gateway port so it is
// correct whether or not this exact process is the one running the gateway (the
// gateway typically runs in a separate `citadel work`); the route wiring is
// handled by exposeWhatsAppGatewayRoute in-process and by the gateway's
// startup self-registration from the persisted BRIDGE_PORT.
func whatsappMeshAPIURL(_ int) string {
	ip := meshIPv4()
	port, useTLS := gatewayPortAndTLS()
	return gatewayRouteURL(useTLS, ip, port, gateway.WhatsAppRoutePrefix)
}

// meshIPv4 resolves this node's mesh IPv4, priming the network singleton the
// same way meshAPIURL (cmd/whatsapp.go) does. Returns "" when off-mesh.
func meshIPv4() string {
	if !network.HasState() {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	network.VerifyOrReconnect(ctx)
	ip, err := network.GetGlobalIPv4()
	if err != nil || ip == "" {
		if st, serr := network.GetGlobalStatus(ctx); serr == nil && st.IPv4 != "" {
			return st.IPv4
		}
		return ""
	}
	return ip
}

// exposeWhatsAppGatewayRoute points the gateway's WhatsApp route at the bridge's
// loopback host port. It is ProvisionDeps.ExposeGatewayRoute for the WhatsApp
// bridge.
//
// In the WHATSAPP_PROVISION worker path the gateway runs IN THIS PROCESS, so we
// wire the route live and it takes effect immediately. In the local CLI path
// (`citadel whatsapp up` without `citadel work` in the same process) there is no
// in-process gateway to update; the route is instead registered by the node's
// separate `citadel work` gateway at startup from the persisted BRIDGE_PORT (see
// registerProvisionedWhatsAppRoute). A nil ref is therefore NOT a hard failure
// here -- but Provision's VerifyReachable step still runs, so if no gateway is
// reachable at all the provision fails loud rather than false-greening.
func exposeWhatsAppGatewayRoute(bridgePort int) error {
	ref := getProvisionedServiceGateway()
	if ref == nil {
		// No in-process gateway; rely on the work gateway's startup
		// self-registration + VerifyReachable. Not an error.
		return nil
	}
	addr := fmt.Sprintf("127.0.0.1:%d", bridgePort)
	if err := ref.gw.SetUpstreamAddress(gateway.WhatsAppRoutePrefix, addr); err != nil {
		return fmt.Errorf("register bridge gateway route %s -> %s: %w", gateway.WhatsAppRoutePrefix, addr, err)
	}
	return nil
}

// registerProvisionedWhatsAppRoute registers the WhatsApp gateway route on gw and,
// if the bridge is already deployed (its env file carries BRIDGE_PORT), points
// the route at that host port. Called from runWork when the gateway is built so:
//   - a bridge provisioned by a PRIOR process (or a prior worker run) is exposed
//     immediately on gateway (re)start, and
//   - the route exists up front so the in-process worker handler only needs
//     SetUpstreamAddress (SetUpstreamAddress errors if the route is absent).
//
// The route is always registered (even with no bridge yet) so a later provision
// can wire it; until then its empty upstream yields a 502, which VerifyReachable
// treats as unreachable.
func registerProvisionedWhatsAppRoute(gw *gateway.Server) {
	// Register the route with an empty upstream; StripPrefix so the bridge's own
	// paths (/health, /qr.txt, /admin/tenants, ...) map through unchanged.
	gw.AddUpstream(gateway.WhatsAppRoutePrefix, &gateway.Upstream{StripPrefix: true})

	// If a bridge is already deployed, wire its persisted port now.
	servicesDir, err := servicesDirForNode()
	if err != nil {
		return
	}
	if !whatsapp.IsDeployed(servicesDir) {
		return
	}
	env, err := whatsapp.LoadEnv(servicesDir)
	if err != nil {
		return
	}
	port := portFromEnv(env)
	if port <= 0 {
		return
	}
	_ = gw.SetUpstreamAddress(gateway.WhatsAppRoutePrefix, fmt.Sprintf("127.0.0.1:%d", port))
}

// verifyBridgeReachable confirms the backend-facing api_url is reachable from
// this node's own mesh identity by GETting the bridge root through it. It is
// ProvisionDeps.VerifyReachable. The bridge's GET / is unauthenticated and
// returns 200 once up, so a 200 (or any non-5xx from the bridge) proves the
// end-to-end mesh path works; a dial failure or a gateway 502 proves it does
// not.
func verifyBridgeReachable(ctx context.Context, apiURL string) error {
	c := meshHTTPClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/", nil)
	if err != nil {
		return fmt.Errorf("build reachability request: %w", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s/: %w", apiURL, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	// A 502 from the gateway means the route has no live upstream (the exact
	// failure we guard against). Any 5xx is treated as unreachable.
	if resp.StatusCode >= 500 {
		return fmt.Errorf("reachability probe to %s/ returned HTTP %d (bridge not exposed on the mesh)", apiURL, resp.StatusCode)
	}
	return nil
}
