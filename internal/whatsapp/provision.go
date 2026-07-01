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
// Port -> DefaultPort).
type ProvisionRequest struct {
	Tenant    string // tenant label (default "default")
	Proxy     string // optional per-tenant egress proxy
	PublicURL string // optional public base URL for copy-pasteable QR links
	Port      int    // host port to publish the bridge on (default DefaultPort)
}

// ProvisionDeps injects the effectful, environment-specific edges so the core
// flow stays unit-testable. Every field is required for a real provision; tests
// stub each with an in-memory implementation.
type ProvisionDeps struct {
	// ServicesDir returns the node's services directory (where the compose +
	// env file live), creating the node config skeleton if needed.
	ServicesDir func() (string, error)

	// DeployCompose ensures the bridge compose is materialized into servicesDir
	// and started (docker compose up -d) with the given env. It mirrors the
	// resolve-source + write-compose + composeUp steps of `citadel whatsapp up`.
	// It must fail fast (never hang) when Docker is unavailable or the private
	// module repo cannot be cloned for lack of credentials.
	DeployCompose func(servicesDir string, env map[string]string) error

	// NewBridgeClient builds a BridgeClient for the locally running bridge at the
	// given loopback port using the admin key.
	NewBridgeClient func(port int, adminKey string) BridgeClient

	// MeshAPIURL returns the api_url the shared backend must use to reach this
	// bridge: the node's mesh (Headscale) IP and the published port. It returns
	// "" when the node is not connected to the mesh, which Provision treats as a
	// hard error (the backend cannot reach a loopback address).
	MeshAPIURL func(port int) string

	// GenerateAdminKey mints a fresh admin secret when none is stored yet.
	// Defaults to GenerateAdminKey.
	GenerateAdminKey func() (string, error)

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
	// AlreadyLinked is true when the tenant already has a live WhatsApp session
	// (no QR is needed).
	AlreadyLinked bool
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
	port := req.Port
	if port <= 0 {
		port = DefaultPort
	}
	readyTimeout := deps.ReadyTimeout
	if readyTimeout <= 0 {
		readyTimeout = 90 * time.Second
	}

	// Resolve the mesh api_url FIRST: the whole point of remote provisioning is
	// to hand the shared backend a reachable address. An off-mesh node can't
	// satisfy that contract, so fail before deploying anything.
	apiURL := deps.MeshAPIURL(port)
	if apiURL == "" {
		return nil, fmt.Errorf("node is not connected to the AceTeam mesh network; cannot determine a reachable api_url (run `citadel login` / bring the node online first)")
	}

	servicesDir, err := deps.ServicesDir()
	if err != nil {
		return nil, fmt.Errorf("resolve node services dir: %w", err)
	}

	env, err := LoadEnv(servicesDir)
	if err != nil {
		return nil, fmt.Errorf("read bridge config: %w", err)
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
	env["BRIDGE_PORT"] = fmt.Sprintf("%d", port)
	if req.Proxy != "" {
		env["DEFAULT_PROXY_URL"] = req.Proxy
	}
	if req.PublicURL != "" {
		env["PUBLIC_URL"] = req.PublicURL
	}

	// Deploy + start the stack. DeployCompose owns source resolution (git clone
	// of the private module repo) and `docker compose up`; it must surface a
	// clear error (not hang) when Docker or the repo credentials are missing.
	log("deploying WhatsApp bridge on port %d", port)
	if err := deps.DeployCompose(servicesDir, env); err != nil {
		return nil, err
	}

	client := deps.NewBridgeClient(port, adminKey)
	log("waiting for the bridge to become ready")
	waitCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	if err := client.WaitReady(waitCtx, readyTimeout); err != nil {
		return nil, fmt.Errorf("bridge did not become ready: %w (check `docker logs citadel-whatsapp-bridge`)", err)
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
		APIURL: apiURL,
		APIKey: tenantKey,
		Tenant: tenant,
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
