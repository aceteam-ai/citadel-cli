// cmd/expose_ops.go
//
// cmd-side live adapter for the EXPOSE_SET worker handler (issue #598). It
// programs the in-process gateway to expose a local service, mints the `link`
// token from the node's persistent signing key, and builds the managed mesh URL.
// It lives here (not in internal/worker) because it needs the in-process gateway
// ref, the node config dir, and the mesh IP — cmd-level edges the worker package
// must not import. Keeping them here lets the worker handler stay unit-testable
// behind the ExposeOps interface (mirrors newLiveModuleOps for MODULE_SET).
package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

// defaultLinkTTL bounds a `link` exposure token when the request omits a TTL. A
// day is long enough to be useful for a shared dashboard link without leaving an
// unbounded credential outstanding.
const defaultLinkTTL = 24 * time.Hour

// liveExposeOps implements worker.ExposeOps against the live gateway.
type liveExposeOps struct{}

// Expose programs the in-process gateway to serve /expose/<name>/ -> the local
// loopback port under the requested visibility, and returns the managed mesh URL
// (plus a signed token for visibility=link). Requires the gateway to run in this
// process (`citadel work --gateway` / `citadel serve`); otherwise it errors so
// the job retries rather than silently no-oping.
func (liveExposeOps) Expose(_ context.Context, req worker.ExposeRequest) (*worker.ExposeResult, error) {
	ref := getProvisionedServiceGateway()
	if ref == nil {
		return nil, fmt.Errorf("no in-process gateway (expose requires the node gateway to be running)")
	}

	policy := &gateway.ExposePolicy{
		Visibility: gateway.Visibility(req.Visibility),
		Creator:    req.Creator,
		TokenEpoch: req.Epoch,
	}
	addr := fmt.Sprintf("127.0.0.1:%d", req.Port)
	if err := ref.gw.Expose(req.Name, addr, policy); err != nil {
		return nil, err
	}

	res := &worker.ExposeResult{URL: exposeMeshURL(req.Name)}

	if policy.Visibility == gateway.VisibilityLink {
		key, err := config.LoadOrCreateExposeSigningKey(platform.ConfigDir())
		if err != nil {
			return nil, fmt.Errorf("load link signing key: %w", err)
		}
		ttl := time.Duration(req.TTLSeconds) * time.Second
		if ttl <= 0 {
			ttl = defaultLinkTTL
		}
		exp := time.Now().Add(ttl)
		res.Token = gateway.MintLinkToken(key, req.Name, req.Epoch, exp)
		res.ExpiresAt = exp.UTC().Format(time.RFC3339)
	}
	return res, nil
}

// exposeMeshURL builds the mesh URL an exposed service is reachable at:
// <scheme>://<vpnIP>:<gatewayPort>/expose/<name>. Returns "" when off-mesh. It
// mirrors gatewayRouteURL/moduleMeshAPIURL for the /expose/ namespace and reads
// the persisted gateway facts so it is correct whether or not this process runs
// the gateway.
func exposeMeshURL(name string) string {
	ip := meshIPv4()
	if ip == "" {
		return ""
	}
	f := gatewayFactsForURL()
	scheme := "https"
	if !f.UseTLS {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s:%d%s", scheme, ip, f.Port, gateway.ExposeRoutePath(name))
}
