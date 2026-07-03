// cmd/provisioned_gateway.go
//
// Generic wiring that makes ANY provisioned, dynamically-ported module reachable
// by the AceTeam backend over the tsnet mesh (aceteam-ai/citadel-cli#447), driven
// entirely by the provisioned-service registry and the module's manifest
// gateway: block. Adding a new module requires ZERO changes here.
//
// The problem: only the status (:8080), terminal (:7860), and HTTPS gateway
// (:8443) servers bind a tsnet listener on the node VPN IP at startup. A
// provisioned module binds an auto-selected FREE host port that nothing on the
// tsnet stack listens on, so advertising http://<vpn-ip>:<module-port> hands the
// backend an unreachable address.
//
// The fix (gateway-route approach): the gateway already binds a tsnet listener on
// the node VPN IP. For each registered module it registers a stable route
// (gateway.ModuleRoutePath(prefix), i.e. /modules/<prefix>) with an EMPTY
// upstream; when the module is deployed on some host port, the route is pointed
// at 127.0.0.1:<port>. The backend then reaches the module at
// <scheme>://<vpn-ip>:<gateway-port>/modules/<prefix> and the gateway strips the
// prefix, forwarding to the module unchanged.
package cmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/provisionedservice"
	"github.com/aceteam-ai/citadel-cli/internal/whatsapp"
)

// DefaultGatewayPort is the gateway's default HTTPS port (matches the
// --gateway-port default in cmd/work.go). Used only as a last-resort fallback
// when the persisted gateway facts file is absent (landmine b): the mesh URL for
// a provisioned module is otherwise read from the file the running `citadel work`
// gateway wrote, so a separate CLI/TUI process advertises the port/TLS the
// gateway actually serves rather than assuming this compile-time default.
const DefaultGatewayPort = 8443

// gatewayFactsFileName is the small node-state file the running gateway writes at
// startup so out-of-process callers (CLI/TUI) build the mesh URL from the ACTUAL
// port + TLS posture + cert path instead of assuming 8443/https (landmine b).
const gatewayFactsFileName = "gateway-facts.json"

// gatewayFacts is the persisted {port, useTLS, certPath} the gateway serves.
// certPath lets the reachability probe trust the gateway's own self-signed cert
// (landmine a) rather than skipping verification.
type gatewayFacts struct {
	Port     int    `json:"port"`
	UseTLS   bool   `json:"use_tls"`
	CertPath string `json:"cert_path"`
}

// provisionedStateDirOverride, when non-empty, replaces network.GetStateDir() as
// the base dir for the gateway-facts file and the provisioned-service registry.
// Tests set it to a temp dir so they never read/write the real node state (which
// GetStateDir resolves from machine-global config/pointer files that a unit test
// cannot cleanly isolate via env vars). Empty in production.
var provisionedStateDirOverride string

// provisionedStateDir returns the base dir for the gateway-facts file and the
// provisioned-service registry, honoring the test override.
func provisionedStateDir() string {
	if provisionedStateDirOverride != "" {
		return provisionedStateDirOverride
	}
	return network.GetStateDir()
}

// gatewayFactsPath is the persisted gateway-facts file inside the node state dir.
func gatewayFactsPath() string {
	return filepath.Join(provisionedStateDir(), gatewayFactsFileName)
}

// writeGatewayFacts persists the live gateway facts so out-of-process callers can
// build a reachable mesh URL and verify against the real cert. Best-effort: a
// write failure only degrades out-of-process URL accuracy to the compile-time
// fallback, it does not stop the gateway.
func writeGatewayFacts(f gatewayFacts) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	path := gatewayFactsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readGatewayFacts loads the persisted gateway facts, or (zero, false) when the
// file is absent/unreadable (the caller then uses the compile-time fallback).
func readGatewayFacts() (gatewayFacts, bool) {
	data, err := os.ReadFile(gatewayFactsPath())
	if err != nil {
		return gatewayFacts{}, false
	}
	var f gatewayFacts
	if err := json.Unmarshal(data, &f); err != nil {
		return gatewayFacts{}, false
	}
	if f.Port <= 0 {
		return gatewayFacts{}, false
	}
	return f, true
}

// provisionedRegistry returns the node's provisioned-service registry, backed by
// a file in the node state dir (the single source of truth for which modules are
// exposed and under which capability).
func provisionedRegistry() *provisionedservice.Registry {
	return provisionedservice.New(provisionedservice.DefaultPath(provisionedStateDir()))
}

// provisionedGatewayRef holds the live gateway server (for in-process route
// wiring) plus the facts needed to build the reachable mesh URL. It is set by
// setProvisionedServiceGateway when runWork starts the in-process gateway.
type provisionedGatewayRef struct {
	gw       *gateway.Server
	port     int    // the gateway's HTTPS/HTTP port (workGatewayPort)
	useTLS   bool   // false only when --gateway-no-tls
	certPath string // path to the gateway's self-signed cert (empty when no-TLS)
}

var (
	provisionedGatewayMu  sync.RWMutex
	provisionedGatewayCur *provisionedGatewayRef
)

// setProvisionedServiceGateway records the live gateway so provisioned-module
// deps can wire routes and compute reachable mesh URLs. Called from runWork after
// the gateway is created. Passing gw==nil clears it.
func setProvisionedServiceGateway(gw *gateway.Server, port int, useTLS bool, certPath string) {
	provisionedGatewayMu.Lock()
	defer provisionedGatewayMu.Unlock()
	if gw == nil {
		provisionedGatewayCur = nil
		return
	}
	provisionedGatewayCur = &provisionedGatewayRef{gw: gw, port: port, useTLS: useTLS, certPath: certPath}
}

// getProvisionedServiceGateway returns the current gateway ref, or nil.
func getProvisionedServiceGateway() *provisionedGatewayRef {
	provisionedGatewayMu.RLock()
	defer provisionedGatewayMu.RUnlock()
	return provisionedGatewayCur
}

// gatewayFactsForURL returns the port + TLS posture + cert path to use when
// building the mesh URL / reachability probe. Precedence: the live in-process
// gateway (exact facts), else the persisted facts file the running `citadel work`
// wrote (landmine b), else the compile-time default (8443, TLS on, no cert -> the
// probe cannot verify and must skip).
func gatewayFactsForURL() gatewayFacts {
	if ref := getProvisionedServiceGateway(); ref != nil {
		return gatewayFacts{Port: ref.port, UseTLS: ref.useTLS, CertPath: ref.certPath}
	}
	if f, ok := readGatewayFacts(); ok {
		return f
	}
	return gatewayFacts{Port: DefaultGatewayPort, UseTLS: true}
}

// gatewayRouteURL builds the mesh URL a module exposed under prefix is reachable
// at: <scheme>://<vpnIP>:<port>/modules/<prefix>. Pure so it is unit-testable.
// Returns "" for an empty vpnIP (off-mesh).
func gatewayRouteURL(useTLS bool, vpnIP string, port int, prefix string) string {
	if vpnIP == "" {
		return ""
	}
	scheme := "https"
	if !useTLS {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s:%d%s", scheme, vpnIP, port, gateway.ModuleRoutePath(prefix))
}

// moduleMeshAPIURL returns the reachable gateway-route mesh URL for the module
// exposed under the given prefix, or "" when the node is off-mesh. It is
// deterministic from the persisted gateway facts (port/TLS), so it is correct
// whether or not this exact process runs the gateway.
func moduleMeshAPIURL(prefix string) string {
	ip := meshIPv4()
	f := gatewayFactsForURL()
	return gatewayRouteURL(f.UseTLS, ip, f.Port, prefix)
}

// whatsappMeshAPIURL is WhatsApp's ProvisionDeps.MeshAPIURL: a thin closure over
// the generic moduleMeshAPIURL. The bridge host port is ignored (the backend
// reaches the module through the gateway route, not the raw port); the argument
// is kept to satisfy the MeshAPIURL(port) signature.
func whatsappMeshAPIURL(_ int) string {
	return moduleMeshAPIURL(whatsappGatewayPrefix)
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

// exposeModuleGatewayRoute points the live gateway's /modules/<prefix> route at
// the module's loopback host port and records the deployment in the registry
// (so a later gateway restart or a separate process picks it up). It is the
// generic ProvisionDeps.ExposeGatewayRoute.
//
// When the gateway runs IN THIS PROCESS (the WHATSAPP_PROVISION worker path) the
// route is wired live and takes effect immediately. In the local CLI path
// (`citadel whatsapp up` without `citadel work` in the same process) there is no
// in-process gateway to update; the registry write is still made, and the running
// gateway's registry watcher (landmine c) picks it up without a restart. A nil
// ref is therefore NOT a hard failure here -- Provision's VerifyReachable step
// still runs, so if no gateway is reachable at all the provision fails loud.
func exposeModuleGatewayRoute(name, prefix, capability string, bridgePort int) error {
	// Persist the exposure first so any gateway (this process's or a separate
	// `citadel work`) can wire it. This is the single source of truth.
	if err := provisionedRegistry().Register(provisionedservice.Entry{
		Name:       name,
		Prefix:     prefix,
		Port:       bridgePort,
		Capability: capability,
	}); err != nil {
		return fmt.Errorf("record module %q in provisioned-service registry: %w", name, err)
	}

	ref := getProvisionedServiceGateway()
	if ref == nil {
		// No in-process gateway; the running gateway's watcher wires it from the
		// registry entry just written. Not an error.
		return nil
	}
	routePrefix := gateway.ModuleRoutePath(prefix)
	addr := fmt.Sprintf("127.0.0.1:%d", bridgePort)
	// The route may not exist yet if this gateway started before the module was
	// declared; register it (empty) then wire it.
	ref.gw.AddUpstream(routePrefix, &gateway.Upstream{StripPrefix: true})
	if err := ref.gw.SetUpstreamAddress(routePrefix, addr); err != nil {
		return fmt.Errorf("wire module gateway route %s -> %s: %w", routePrefix, addr, err)
	}
	return nil
}

// exposeWhatsAppGatewayRoute is WhatsApp's ProvisionDeps.ExposeGatewayRoute: a
// thin closure over the generic exposeModuleGatewayRoute with WhatsApp's manifest
// defaults.
func exposeWhatsAppGatewayRoute(bridgePort int) error {
	return exposeModuleGatewayRoute(whatsapp.ServiceName, whatsappGatewayPrefix, whatsappGatewayCapability, bridgePort)
}

// registerProvisionedModuleRoutes registers a /modules/<prefix> route on gw for
// EVERY module recorded in the provisioned-service registry, wiring the route to
// the module's persisted port when known. Called from runWork when the gateway is
// built so a module provisioned by a PRIOR process (or a prior worker run) is
// exposed immediately on gateway (re)start. Returns the entries it registered so
// the caller can print them in the route table.
//
// The WhatsApp bridge predates the registry, so this also self-heals: if the
// bridge is deployed (its env file carries BRIDGE_PORT) but has no registry entry
// yet, it is registered here with WhatsApp's manifest defaults so existing
// deployments keep working without any manual migration.
func registerProvisionedModuleRoutes(gw *gateway.Server) []provisionedservice.Entry {
	backfillWhatsAppRegistryEntry()

	entries, err := provisionedRegistry().List()
	if err != nil {
		Log("provisioned-service registry read failed: %v", err)
		return nil
	}
	for _, e := range entries {
		routePrefix := gateway.ModuleRoutePath(e.Prefix)
		gw.AddUpstream(routePrefix, &gateway.Upstream{StripPrefix: true})
		if e.Port > 0 {
			_ = gw.SetUpstreamAddress(routePrefix, fmt.Sprintf("127.0.0.1:%d", e.Port))
		}
	}
	return entries
}

// backfillWhatsAppRegistryEntry ensures a deployed-but-unregistered WhatsApp
// bridge (from before the registry existed) gets a registry entry with the
// WhatsApp manifest defaults, so existing deployments are exposed after upgrade
// without any operator action. No-op when the bridge is not deployed or already
// registered.
func backfillWhatsAppRegistryEntry() {
	servicesDir, err := servicesDirForNode()
	if err != nil {
		return
	}
	if !whatsapp.IsDeployed(servicesDir) {
		return
	}
	reg := provisionedRegistry()
	if _, found := reg.CapabilityForPrefix(whatsappGatewayPrefix); found {
		return // already registered
	}
	env, err := whatsapp.LoadEnv(servicesDir)
	if err != nil {
		return
	}
	port := portFromEnv(env)
	if port <= 0 {
		return
	}
	_ = reg.Register(provisionedservice.Entry{
		Name:       whatsapp.ServiceName,
		Prefix:     whatsappGatewayPrefix,
		Port:       port,
		Capability: whatsappGatewayCapability,
	})
}

// watchProvisionedRegistry polls the registry file and registers/wires any module
// that appears (or whose port changes) while the gateway is already running
// (landmine c: a module provisioned via the CLI while `citadel work` is up would
// otherwise never get wired until a restart). A lightweight poll is used rather
// than a file-watch dependency: the registry changes rarely and the poll is cheap
// (a small JSON read). It exits when ctx is cancelled.
func watchProvisionedRegistry(ctx context.Context, gw *gateway.Server) {
	reg := provisionedRegistry()
	known := map[string]int{} // prefix -> wired port
	// Seed with what registerProvisionedModuleRoutes already wired at startup.
	if entries, err := reg.List(); err == nil {
		for _, e := range entries {
			known[e.Prefix] = e.Port
		}
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			entries, err := reg.List()
			if err != nil {
				continue
			}
			for _, e := range entries {
				prev, seen := known[e.Prefix]
				if seen && prev == e.Port {
					continue
				}
				routePrefix := gateway.ModuleRoutePath(e.Prefix)
				gw.AddUpstream(routePrefix, &gateway.Upstream{StripPrefix: true})
				if e.Port > 0 {
					if err := gw.SetUpstreamAddress(routePrefix, fmt.Sprintf("127.0.0.1:%d", e.Port)); err == nil {
						Log("gateway picked up provisioned module %q on /modules/%s -> 127.0.0.1:%d", e.Name, e.Prefix, e.Port)
					}
				}
				known[e.Prefix] = e.Port
			}
		}
	}
}

// verifyModuleReachable confirms the backend-facing api_url is reachable from
// this node's own mesh identity by GETting the module root through it. It is the
// generic ProvisionDeps.VerifyReachable.
//
// Landmine a: it does NOT use InsecureSkipVerify. The gateway serves a self-signed
// cert; a correctly-configured consumer verifies TLS against it. So this probe
// mirrors that consumer -- it loads the gateway's own cert (from the persisted
// facts) into a CA pool and verifies against it (ServerName = the node VPN IP).
// If verification fails, it fails loud, matching what the real backend would see.
// (Only when the gateway runs --gateway-no-tls does the URL become http and no
// verification is needed.) The remaining BACKEND-SIDE dependency -- the backend
// must trust this same cert (or the node runs --gateway-no-tls) -- is flagged in
// the PR, not papered over.
func verifyModuleReachable(ctx context.Context, apiURL string) error {
	client, err := meshVerifyingClient()
	if err != nil {
		return fmt.Errorf("build reachability client: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/", nil)
	if err != nil {
		return fmt.Errorf("build reachability request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s/: %w", apiURL, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	// A 502 from the gateway means the route has no live upstream (the exact
	// failure we guard against). Any 5xx is treated as unreachable.
	if resp.StatusCode >= 500 {
		return fmt.Errorf("reachability probe to %s/ returned HTTP %d (module not exposed on the mesh)", apiURL, resp.StatusCode)
	}
	return nil
}

// verifyBridgeReachable is WhatsApp's ProvisionDeps.VerifyReachable: an alias for
// the generic verifyModuleReachable.
func verifyBridgeReachable(ctx context.Context, apiURL string) error {
	return verifyModuleReachable(ctx, apiURL)
}

// meshVerifyingClient builds an HTTP client whose TLS config VERIFIES the
// gateway's self-signed cert (landmine a), loaded from the persisted gateway
// facts, instead of skipping verification. When the gateway runs without TLS
// (no cert on disk) a plain client is returned (the URL is http). When TLS is on
// but the cert cannot be loaded, it returns an error rather than silently
// skipping verification -- a false-green must never re-enter here.
func meshVerifyingClient() (*http.Client, error) {
	f := gatewayFactsForURL()
	if !f.UseTLS {
		return &http.Client{Timeout: 10 * time.Second}, nil
	}
	if f.CertPath == "" {
		return nil, fmt.Errorf("gateway TLS is on but its certificate path is unknown (is `citadel work` running with the gateway?); cannot verify reachability without trusting the gateway cert")
	}
	pem, err := os.ReadFile(f.CertPath)
	if err != nil {
		return nil, fmt.Errorf("read gateway cert %s: %w", f.CertPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("gateway cert %s is not valid PEM", f.CertPath)
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				ServerName: meshIPv4(), // the SAN the gateway cert carries for the mesh
				MinVersion: tls.VersionTLS12,
			},
		},
	}, nil
}
