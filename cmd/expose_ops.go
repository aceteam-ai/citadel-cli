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
	"net"
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

	// Persist so the exposure survives a worker restart (#647). Every caller --
	// the CLI, the MCP verb, the EXPOSE_SET job -- funnels through here, so this
	// is the one place the durable set can be kept in step with the live one. A
	// write failure is logged, not returned: the exposure IS live and the caller
	// already has its URL, so failing the call would be a lie; the honest failure
	// mode is "works now, gone after a restart", and it must be visible.
	if err := config.SaveExposure(platform.ConfigDir(), config.ExposureRecord{
		Name:       req.Name,
		Port:       req.Port,
		Visibility: req.Visibility,
		Creator:    req.Creator,
		TokenEpoch: req.Epoch,
	}); err != nil {
		Log("warning: exposure %q is live but was not persisted (it will not survive a restart): %v", req.Name, err)
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

// restoreExposures re-wires the persisted exposure set onto a gateway that has
// not started serving yet (#647). It MUST be called before Start: restoring
// after the listener is up leaves a window in which a valid exposure 404s, which
// is exactly the symptom this fixes.
//
// Every failure here is non-fatal and logged. A node whose exposure store is
// unreadable must still come up serving its builtin routes; refusing to start
// would turn a lost side-feature into an outage.
func restoreExposures(gw *gateway.Server) {
	recs, err := config.LoadExposures(platform.ConfigDir())
	if err != nil {
		Log("warning: could not restore gateway exposures: %v", err)
		return
	}
	if len(recs) == 0 {
		return
	}

	restored := 0
	for _, r := range recs {
		policy := &gateway.ExposePolicy{
			Visibility: gateway.Visibility(r.Visibility),
			Creator:    r.Creator,
			TokenEpoch: r.TokenEpoch,
		}
		addr := fmt.Sprintf("127.0.0.1:%d", r.Port)
		if err := gw.Expose(r.Name, addr, policy); err != nil {
			// A record the gateway rejects (unknown visibility, bad name) is data
			// we cannot honor -- skip it loudly rather than dropping the whole set.
			Log("warning: skipping persisted exposure %q: %v", r.Name, err)
			continue
		}
		// The upstream may be gone after a reboot (module removed, port changed).
		// Say so now: otherwise the route restores fine and every request 502s
		// with nothing explaining why.
		if !localPortListening(r.Port) {
			Log("warning: exposure %q restored but nothing is listening on 127.0.0.1:%d yet", r.Name, r.Port)
		}
		restored++
	}
	if restored > 0 {
		Log("Restored %d gateway exposure(s)", restored)
	}
}

// localPortListening reports whether something accepts TCP on the loopback port.
// Short timeout: this runs once per exposure on the startup path, and a slow or
// filtered probe must never delay the gateway coming up.
func localPortListening(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
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
