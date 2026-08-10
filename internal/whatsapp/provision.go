// internal/whatsapp/provision.go
//
// Shared "deploy + mint + wait + fetch QR" orchestration for the WhatsApp
// bridge. This is the exact flow `citadel whatsapp up` performs locally, lifted
// out of cmd/whatsapp.go so the WHATSAPP_PROVISION node-job handler
// (internal/worker) can drive the same steps remotely without reimplementing
// the bridge.
//
// The effectful edges the CLI owns -- resolving the module source (git clone),
// running docker compose, and discovering the node's mesh IP -- are injected as
// function fields (ProvisionDeps). That keeps this package free of the catalog /
// network / docker dependencies (no import cycle) and lets the handler unit-test
// the whole flow with in-memory stubs.
package whatsapp

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"rsc.io/qr"
)

// BridgeClient is the subset of *Client the provision flow needs. Declaring it
// as an interface lets the handler inject a fake bridge in tests while the CLI
// and handler pass a real *Client (which satisfies it).
type BridgeClient interface {
	WaitReady(ctx context.Context, timeout time.Duration) error
	CreateTenant(ctx context.Context, name, proxyURL string) (*Tenant, error)
	Health(ctx context.Context, apiKey string) (*Health, error)
	QRString(ctx context.Context, apiKey string) (string, error)
}

// Ensure the concrete client satisfies the interface.
var _ BridgeClient = (*Client)(nil)

// ProvisionRequest carries the caller-supplied knobs. All fields are optional;
// zero values fall back to sensible defaults (Tenant -> "default",
// Port -> auto-selected free host port).
type ProvisionRequest struct {
	Tenant    string // tenant label (default "default")
	Proxy     string // optional per-tenant egress proxy
	PublicURL string // optional public base URL for copy-pasteable QR links
	// Port is the host port to publish the bridge on. When <= 0 (the common
	// case), Provision auto-selects a FREE host port so it does not collide with
	// citadel's own 8080 listener (aceteam-ai/citadel-cli#438). A positive value
	// is an explicit operator override and is honored verbatim.
	Port int
}

// ProvisionDeps injects the effectful, environment-specific edges so the core
// flow stays unit-testable. Every field is required for a real provision; tests
// stub each with an in-memory implementation.
type ProvisionDeps struct {
	// ServicesDir returns the node's services directory (where the compose +
	// env file live), creating the node config skeleton if needed.
	ServicesDir func() (string, error)

	// DeployCompose ensures the bridge compose is materialized into servicesDir
	// and started (docker compose pull + up -d) with the given env. It mirrors the
	// resolve-source + write-compose + composePull + composeUp steps of
	// `citadel whatsapp up`. It must fail fast (never hang) when Docker is
	// unavailable or the private module repo cannot be cloned for lack of
	// credentials.
	//
	// It MUST refresh the bridge image before starting. The compose pins a
	// floating `:latest` tag, so a bare `up -d` with that tag already present
	// locally is a no-op and a provisioned bridge can never be upgraded
	// (aceteam-ai/citadel-cli#718).
	DeployCompose func(servicesDir string, env map[string]string) error

	// BridgeImageID reports the docker image ID (sha256:...) the bridge container
	// is CURRENTLY running, or "" when the bridge is not running or Docker is
	// unavailable. Provision samples it immediately before and immediately after
	// DeployCompose so the result can distinguish a real upgrade from a no-op
	// re-provision (#718). It must read the RUNNING CONTAINER's image, not the
	// local tag: after a pull the tag already points at the new image while the
	// container still runs the old one until it is recreated, so a tag-derived
	// value would report an upgrade that did not happen. Nil leaves the image
	// fields empty and Upgraded false (never a fabricated "upgraded").
	BridgeImageID func() string

	// ImagePullError returns the error text of the image pull DeployCompose
	// attempted, or "" when it succeeded (or was not attempted). A pull failure
	// is deliberately NOT fatal -- a node without `docker login` for the private
	// registry can still run its locally cached image, and failing would regress
	// re-provisions that work today -- but it MUST be visible, because a pull
	// that silently did nothing is exactly the false-green #718 is about. Nil
	// leaves ProvisionResult.ImagePullError empty.
	ImagePullError func() string

	// NewBridgeClient builds a BridgeClient for the locally running bridge at the
	// given loopback port using the admin key.
	NewBridgeClient func(port int, adminKey string) BridgeClient

	// MeshAPIURL returns the api_url the shared backend must use to reach this
	// bridge over the mesh. It MUST return an actually-reachable URL -- the
	// gateway route form (https://<node-vpn-ip>:<gateway-port>/modules/<prefix>),
	// NOT a raw http://<vpn-ip>:<bridge-port> that nothing on the tsnet stack listens
	// on. Only the status/terminal/gateway servers bind a tsnet listener on the
	// node VPN IP; a provisioned bridge on an auto-selected host port is reached
	// only through the gateway route (aceteam-ai/citadel-cli#447).
	//
	// The port argument is the bridge's host port; the returned URL is the
	// gateway route (independent of that port -- the gateway strips the prefix
	// and forwards to the bridge port set via ExposeGatewayRoute). It returns ""
	// when the node is not connected to the mesh, which Provision treats as a
	// hard error (the backend cannot reach a loopback address).
	MeshAPIURL func(port int) string

	// ExposeGatewayRoute points the gateway's provisioned-service route at the
	// bridge's loopback host port so the mesh URL returned by MeshAPIURL is
	// actually reachable. It is the side effect that makes the bridge reachable
	// on the tsnet mesh (the gateway already binds a tsnet listener on the node
	// VPN IP; this just wires the /modules/<prefix> route to 127.0.0.1:<bridgePort>).
	// Nil means "no gateway to register with" -- Provision then relies purely on
	// VerifyReachable to catch an unreachable bridge (fail loud, not false-green).
	ExposeGatewayRoute func(bridgePort int) error

	// VerifyReachable confirms the backend-facing api_url is actually reachable
	// from the node's own mesh identity (e.g. GET the bridge root through the
	// gateway route). Provision calls it after deploy + route-exposure and BEFORE
	// computing already_linked / returning success, so an unexposed or wedged
	// bridge fails loud with an actionable error instead of a structural
	// false-green (aceteam-ai/citadel-cli#447). Nil skips the check (used by unit
	// tests that stub the whole flow); real wiring always provides it.
	VerifyReachable func(ctx context.Context, apiURL string) error

	// GenerateAdminKey mints a fresh admin secret when none is stored yet.
	// Defaults to GenerateAdminKey.
	GenerateAdminKey func() (string, error)

	// SelectHostPort chooses the host port the bridge publishes on when the
	// request does not pin one (ProvisionRequest.Port <= 0). It must return a
	// port that can actually be bound on the host, so the bridge does not collide
	// with citadel's own 8080 listener (aceteam-ai/citadel-cli#438). preferred is
	// the explicit override (honored verbatim when > 0); floor is the minimum port
	// to scan from, used so a bind-collision retry resumes above the failed port.
	// Nil defaults to SelectHostPort (a live bind probe). Injectable so tests can
	// simulate an occupied port without touching the network stack.
	SelectHostPort func(preferred, floor int) (int, error)

	// GatewayCertPEM returns the current gateway self-signed leaf cert PEM the
	// backend must trust to reach the https gateway-route api_url. It returns ""
	// when the gateway runs without TLS or the cert is unavailable (the backend
	// then uses plain http). Nil leaves ProvisionResult.GatewayCertPEM empty (so
	// older wiring without this dep degrades gracefully to no-cert).
	GatewayCertPEM func() string

	// CertRefreshURL returns the plaintext URL the backend re-fetches the gateway
	// cert from on rotation (http://<mesh-ip>:<status-port>/gateway-cert.pem), or
	// "" when off-mesh / status port unknown. Nil leaves the field empty.
	CertRefreshURL func() string

	// ReadyTimeout bounds the WaitReady poll. Zero uses 90s.
	ReadyTimeout time.Duration

	// Log reports progress. Nil is a no-op.
	Log func(format string, args ...any)
}

// ProvisionResult is the structured outcome of a provision, shared verbatim with
// the WHATSAPP_PROVISION node job's return document.
type ProvisionResult struct {
	// APIURL is the mesh-IP base URL the backend registers (http://<mesh-ip>:<port>).
	APIURL string
	// APIKey is the per-tenant data-plane key (wab_...) minted here.
	APIKey string
	// QR is the pairing QR payload string from the bridge (empty when already linked).
	QR string
	// Tenant is the tenant label.
	Tenant string
	// Port is the host port the bridge was published on. It is the auto-selected
	// free port (or the explicit ProvisionRequest.Port override) and is the port
	// embedded in APIURL, so callers that print connect details use the real
	// port rather than assuming DefaultPort.
	Port int
	// AlreadyLinked is true when the tenant already has a live WhatsApp session
	// (no QR is needed).
	AlreadyLinked bool
	// GatewayCertPEM is the current gateway self-signed leaf cert PEM the backend
	// must trust to reach APIURL (which is an https gateway route). Empty when the
	// gateway runs without TLS (--gateway-no-tls) or the cert is unavailable, in
	// which case the backend uses plain http. A public leaf cert, safe to serve.
	GatewayCertPEM string
	// CertRefreshURL is the plaintext URL the backend re-fetches the gateway cert
	// from on rotation: http://<mesh-ip>:<status-port>/gateway-cert.pem. Empty when
	// the node is off-mesh or the status server's port is unknown.
	CertRefreshURL string
	// ImageIDBefore is the docker image ID the bridge container was running before
	// the deploy, or "" when the bridge was not running (a first deploy) or the
	// image could not be read. Together with ImageIDAfter it makes the outcome
	// legible: an `already_linked` result on an unchanged image is a no-op, not an
	// upgrade (aceteam-ai/citadel-cli#718).
	ImageIDBefore string
	// ImageIDAfter is the docker image ID the bridge container is running after the
	// deploy, or "" when it could not be read.
	ImageIDAfter string
	// Upgraded is true only when BOTH image IDs were readable AND they differ --
	// i.e. the deploy actually moved the bridge onto a new image. A first deploy
	// (no "before" container) and an unreadable image both report false: an
	// unknown is never reported as an upgrade.
	Upgraded bool
	// ImagePullError is the error text of the pre-deploy image pull, or "" when it
	// succeeded or was not attempted. Non-empty means the bridge is running the
	// locally cached image and an upgrade could NOT have happened -- the provision
	// still succeeds (the bridge is up and linked), but the caller must not read
	// that success as "running the latest image".
	ImagePullError string
}

// persistedBridgePort returns the bridge's previously-persisted BRIDGE_PORT from
// the env map, or 0 when none is recorded or it is not a valid positive port. It
// deliberately does NOT default to DefaultPort (8080 = citadel's own listener):
// a re-provision must reuse only a port the deployment actually chose, never a
// coincidental default that would misroute onto citadel's own services.
func persistedBridgePort(env map[string]string) int {
	v := env["BRIDGE_PORT"]
	if v == "" {
		return 0
	}
	p, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || p <= 0 {
		return 0
	}
	return p
}

// Provision runs the full deploy -> mint -> wait -> fetch-QR flow and returns the
// connection details. It is the single source of truth shared by the CLI
// (`citadel whatsapp up`) and the WHATSAPP_PROVISION node job.
//
// It resolves the mesh api_url up front so an off-mesh node fails cleanly rather
// than handing the backend an unreachable loopback address.
func Provision(ctx context.Context, req ProvisionRequest, deps ProvisionDeps) (*ProvisionResult, error) {
	if deps.ServicesDir == nil || deps.DeployCompose == nil || deps.NewBridgeClient == nil || deps.MeshAPIURL == nil {
		return nil, fmt.Errorf("whatsapp.Provision: ServicesDir, DeployCompose, NewBridgeClient and MeshAPIURL are required")
	}
	log := deps.Log
	if log == nil {
		log = func(string, ...any) {}
	}
	genAdminKey := deps.GenerateAdminKey
	if genAdminKey == nil {
		genAdminKey = GenerateAdminKey
	}
	tenant := req.Tenant
	if tenant == "" {
		tenant = "default"
	}
	servicesDir, err := deps.ServicesDir()
	if err != nil {
		return nil, fmt.Errorf("resolve node services dir: %w", err)
	}

	env, err := LoadEnv(servicesDir)
	if err != nil {
		return nil, fmt.Errorf("read bridge config: %w", err)
	}

	// Choose the host port. Precedence:
	//   1. An explicit ProvisionRequest.Port is honored verbatim (operator override).
	//   2. Otherwise, on a RE-PROVISION, reuse the persisted BRIDGE_PORT so we do
	//      not churn to a new host port every time (#449). The old bridge still
	//      holds that port at probe time, so a free-port scan would skip it and
	//      pick a new one -- and `compose up` on the same project rebinds the same
	//      port cleanly (it recreates the container). Reusing it keeps the mesh
	//      gateway route stable across re-provisions.
	//   3. Otherwise auto-select a FREE host port so a first deploy does not collide
	//      with citadel's own 8080 listener (aceteam-ai/citadel-cli#438).
	// A bind collision on the reused port (e.g. a foreign process grabbed it while
	// the bridge was down) still falls through to the auto-select retry below.
	selectPort := deps.SelectHostPort
	if selectPort == nil {
		selectPort = func(preferred, floor int) (int, error) { return SelectHostPort(preferred, floor, nil) }
	}
	preferredPort := req.Port
	if preferredPort <= 0 {
		if persisted := persistedBridgePort(env); persisted > 0 {
			preferredPort = persisted
			log("reusing persisted bridge host port %d (idempotent re-provision)", persisted)
		}
	}
	port, err := selectPort(preferredPort, 0)
	if err != nil {
		return nil, fmt.Errorf("select bridge host port: %w", err)
	}

	readyTimeout := deps.ReadyTimeout
	if readyTimeout <= 0 {
		readyTimeout = 90 * time.Second
	}

	// Resolve the mesh api_url FIRST (before deploying): the whole point of remote
	// provisioning is to hand the shared backend a reachable address. An off-mesh
	// node can't satisfy that contract. The port may still change below if a
	// bind-collision retry picks a new one, so apiURL is recomputed after deploy.
	if deps.MeshAPIURL(port) == "" {
		return nil, fmt.Errorf("node is not connected to the AceTeam mesh network; cannot determine a reachable api_url (run `citadel login` / bring the node online first)")
	}

	adminKey := env["ADMIN_API_KEY"]
	if adminKey == "" {
		adminKey, err = genAdminKey()
		if err != nil {
			return nil, err
		}
		log("generated a new admin secret for the bridge control plane")
	}
	env["ADMIN_API_KEY"] = adminKey
	if req.Proxy != "" {
		env["DEFAULT_PROXY_URL"] = req.Proxy
	}
	if req.PublicURL != "" {
		env["PUBLIC_URL"] = req.PublicURL
	}

	// Deploy + start the stack. DeployCompose owns source resolution (git clone
	// of the private module repo) and `docker compose up`; it must surface a
	// clear error (not hang) when Docker or the repo credentials are missing.
	//
	// SelectHostPort only *probed* the port; between probe and `compose up`
	// another process can grab it (TOCTOU). When the port was auto-selected we
	// treat a bind-collision as recoverable: re-select a fresh free port and
	// retry a bounded number of times. An explicit operator override is never
	// silently moved -- its bind error surfaces as-is.
	// Sample the image the bridge is running BEFORE the deploy. Paired with the
	// post-deploy sample below this is what lets a caller tell a real upgrade from
	// a no-op `up -d` (aceteam-ai/citadel-cli#718). Best-effort: "" (bridge not
	// running / Docker unreadable) simply means "unknown", never "upgraded".
	imageIDBefore := ""
	if deps.BridgeImageID != nil {
		imageIDBefore = deps.BridgeImageID()
	}

	const maxDeployAttempts = 4
	autoSelected := req.Port <= 0
	for attempt := 1; ; attempt++ {
		env["BRIDGE_PORT"] = fmt.Sprintf("%d", port)
		log("deploying WhatsApp bridge on port %d", port)
		derr := deps.DeployCompose(servicesDir, env)
		if derr == nil {
			break
		}
		if !autoSelected || attempt >= maxDeployAttempts || !isHostPortCollision(derr) {
			return nil, derr
		}
		// The just-tried port is now known-occupied; re-select from the next
		// candidate above it (floor = port+1) so we don't immediately re-pick it.
		log("port %d was taken between probe and bind; selecting another (%v)", port, derr)
		next, serr := selectPort(0, port+1)
		if serr != nil {
			return nil, fmt.Errorf("retry after host-port collision on %d: %w", port, serr)
		}
		port = next
	}

	// Sample the image again now the deploy has returned. Compare on the CONTAINER's
	// image, so "upgraded" means the container was actually recreated onto a new
	// image -- not merely that a newer tag was pulled into the local store.
	imageIDAfter := ""
	if deps.BridgeImageID != nil {
		imageIDAfter = deps.BridgeImageID()
	}
	upgraded := imageIDBefore != "" && imageIDAfter != "" && imageIDBefore != imageIDAfter
	pullErr := ""
	if deps.ImagePullError != nil {
		pullErr = deps.ImagePullError()
	}
	switch {
	case pullErr != "":
		log("warning: could not refresh the bridge image (%s); the bridge is running its locally cached image, so this provision could NOT have upgraded it", pullErr)
	case upgraded:
		log("bridge image upgraded: %s -> %s", imageIDBefore, imageIDAfter)
	case imageIDAfter != "" && imageIDBefore != "":
		log("bridge image unchanged (%s); already up to date", imageIDAfter)
	}

	// Recompute the mesh api_url from the port we actually published on (a
	// bind-collision retry may have moved it). The pre-deploy check above already
	// proved the node is on the mesh; a now-empty result would be a transient
	// network blip, so fall back to the same reachable IP with the final port.
	apiURL := deps.MeshAPIURL(port)
	if apiURL == "" {
		return nil, fmt.Errorf("node left the AceTeam mesh network during provisioning; cannot determine a reachable api_url")
	}

	// Expose the bridge on the tsnet-reachable surface: point the gateway's
	// provisioned-service route at the bridge's loopback host port. Without this
	// the api_url above resolves to the gateway's VPN listener but the route has
	// no upstream, so the backend gets a 502 -- the exact "bridge unreachable"
	// this fixes (aceteam-ai/citadel-cli#447). A registration failure is fatal:
	// returning success with an unreachable api_url is the false-green we are
	// eliminating.
	if deps.ExposeGatewayRoute != nil {
		if err := deps.ExposeGatewayRoute(port); err != nil {
			return nil, fmt.Errorf("expose bridge on the mesh gateway: %w", err)
		}
	}

	client := deps.NewBridgeClient(port, adminKey)
	log("waiting for the bridge to become ready")
	waitCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	if err := client.WaitReady(waitCtx, readyTimeout); err != nil {
		return nil, fmt.Errorf("bridge did not become ready: %w (check `docker compose -p %s logs %s`)", err, ProjectName(servicesDir), BridgeService)
	}

	// Verify the backend-facing api_url is actually reachable from the node's own
	// mesh identity BEFORE returning success or computing already_linked. The
	// loopback bridge being up (WaitReady, above) does NOT prove the mesh path
	// works: the gateway route or the tsnet listener could be missing. This is
	// the check that turns the old structural false-green (report success on an
	// unreachable bridge) into a loud, actionable failure (#447).
	if deps.VerifyReachable != nil {
		if err := deps.VerifyReachable(waitCtx, apiURL); err != nil {
			return nil, fmt.Errorf("bridge is up locally but its mesh api_url %s is NOT reachable: %w -- the backend would not be able to reach it. Ensure `citadel work` is running with the gateway enabled (it exposes provisioned services on the mesh)", apiURL, err)
		}
	}

	// Mint (or reuse) the tenant. Reusing a previously minted tenant preserves an
	// already-linked WhatsApp session and its api_key -- minting a fresh one would
	// orphan the linked session and rotate the key the user already registered.
	tenantKey := env["TENANT_API_KEY"]
	if tenantKey == "" {
		t, err := client.CreateTenant(waitCtx, tenant, req.Proxy)
		if err != nil {
			return nil, fmt.Errorf("provision tenant: %w", err)
		}
		tenantKey = t.APIKey
		env["TENANT_API_KEY"] = t.APIKey
		env["TENANT_ID"] = t.ID
		env["TENANT_NAME"] = t.Name
		if t.Name != "" {
			tenant = t.Name
		}
	} else {
		log("reusing the existing tenant (its linked session and api_key are preserved)")
	}

	// Persist the updated env (admin key + tenant details) best-effort. A save
	// failure must not fail the provision -- the caller already has the details.
	if err := SaveEnv(servicesDir, env); err != nil {
		log("warning: could not persist bridge config: %v", err)
	}

	result := &ProvisionResult{
		APIURL:         apiURL,
		APIKey:         tenantKey,
		Tenant:         tenant,
		Port:           port,
		ImageIDBefore:  imageIDBefore,
		ImageIDAfter:   imageIDAfter,
		Upgraded:       upgraded,
		ImagePullError: pullErr,
	}
	// Publish the gateway cert (so the backend can trust the https api_url) and the
	// plaintext refresh URL (so it re-fetches on rotation). Both deps are optional:
	// a nil dep leaves the corresponding field empty, which the backend treats as
	// "no cert / use http" and "no refresh channel".
	if deps.GatewayCertPEM != nil {
		result.GatewayCertPEM = deps.GatewayCertPEM()
	}
	if deps.CertRefreshURL != nil {
		result.CertRefreshURL = deps.CertRefreshURL()
	}

	// If the tenant is already linked, there is no QR to scan.
	if h, err := client.Health(waitCtx, tenantKey); err == nil && h.LoggedIn {
		result.AlreadyLinked = true
		return result, nil
	}

	// Fetch the pairing QR. An empty payload from the bridge also means "already
	// linked" (the bridge returns "" once a session is live).
	qr, err := client.QRString(waitCtx, tenantKey)
	if err != nil {
		// The bridge is up and the tenant is minted; a transient QR-fetch failure
		// should not sink the whole provision. Return without a QR and let the
		// caller retry (the backend can re-fetch via the same api_url + api_key).
		log("warning: could not fetch pairing QR yet: %v", err)
		return result, nil
	}
	if qr == "" {
		result.AlreadyLinked = true
		return result, nil
	}
	result.QR = qr
	return result, nil
}

// QRDataURL renders a QR payload string as a base64 "data:image/png;base64,..."
// URL so the server can surface the pairing QR to the human without the phone
// (or the backend) ever reaching the bridge directly. Returns "" for an empty
// payload (an already-linked tenant has no QR).
func QRDataURL(payload string) (string, error) {
	if payload == "" {
		return "", nil
	}
	// Level M matches the density/robustness used elsewhere in the codebase
	// (internal/ui/qrcode.go) and is what WhatsApp's linking QR expects.
	code, err := qr.Encode(payload, qr.M)
	if err != nil {
		return "", fmt.Errorf("encode QR: %w", err)
	}
	png := code.PNG()
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}
