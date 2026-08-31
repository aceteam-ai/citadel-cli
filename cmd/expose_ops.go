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
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

// defaultLinkTTL bounds a `link` exposure token when the request omits a TTL. A
// day is long enough to be useful for a shared dashboard link without leaving an
// unbounded credential outstanding.
const defaultLinkTTL = 24 * time.Hour

// liveExposeOps implements worker.ExposeOps against the live gateway.
type liveExposeOps struct{}

// Expose programs the in-process gateway to serve /expose/<name>/ under the
// requested visibility, and returns the managed mesh URL (plus a signed token
// for visibility=link). Requires the gateway to run in this process (`citadel
// work --gateway` / `citadel serve`); otherwise it errors so the job retries
// rather than silently no-oping.
//
// Two mutually exclusive source types (issue #943): req.Port reverse-proxies a
// local loopback service; req.Path serves a workspace-confined static
// directory directly from the gateway. ExposeSetHandler.parseExposeRequest
// already enforces exactly one is set before this is ever called.
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

	rec := config.ExposureRecord{
		Name:       req.Name,
		Visibility: req.Visibility,
		Creator:    req.Creator,
		TokenEpoch: req.Epoch,
	}

	if req.Path != "" {
		// Directory source (#943). Confine req.Path to the node workspace
		// BEFORE it ever reaches the gateway -- the same boundary FILE_READ/
		// FILE_LIST enforce, and deliberately NOT the AllowReadOutsideWorkspace
		// relaxation those handlers can opt into: a network-reachable share is
		// workspace-pinned regardless of that flag. jobs.ValidatePath also
		// resolves symlinks on the root itself, so the gateway's own per-request
		// confinement (internal/gateway/expose_dir.go) starts from an already
		// clean boundary.
		resolvedRoot, err := jobs.ValidatePath(resolveWorkspaceDir(), req.Path)
		if err != nil {
			return nil, fmt.Errorf("expose path %q: %w", req.Path, err)
		}
		if err := ref.gw.ExposeDir(req.Name, resolvedRoot, policy); err != nil {
			return nil, err
		}
		rec.Path = resolvedRoot
	} else {
		addr := fmt.Sprintf("127.0.0.1:%d", req.Port)
		if err := ref.gw.Expose(req.Name, addr, policy); err != nil {
			return nil, err
		}
		rec.Port = req.Port
	}

	// Persist so the exposure survives a worker restart (#647). Every caller --
	// the CLI, the MCP verb, the EXPOSE_SET job -- funnels through here, so this
	// is the one place the durable set can be kept in step with the live one. A
	// write failure is logged, not returned: the exposure IS live and the caller
	// already has its URL, so failing the call would be a lie; the honest failure
	// mode is "works now, gone after a restart", and it must be visible.
	if err := config.SaveExposure(platform.ConfigDir(), rec); err != nil {
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

// UnexposeResult is what the unexpose path returns to its caller (the CLI, the
// aceteam MCP verb).
type UnexposeResult struct {
	// Name is the exposed-service slug that was revoked.
	Name string `json:"name"`
	// WasExposed reports whether a live exposure actually existed. False means
	// the call still succeeded (revoke is idempotent) but nothing was serving --
	// surfaced so a caller can say "not exposed" instead of implying it tore
	// something down.
	WasExposed bool `json:"was_exposed"`
}

// Unexpose revokes an exposure: it drops the gateway's live route AND the
// durable record, so the service stops being reachable now and does not come
// back on the next restart (#647 made exposures survive restarts, which is
// exactly why revoke must clear both halves).
//
// Order matters. The LIVE route is torn down first: if the durable delete fails,
// the service is already unreachable and the residual failure is a stale record
// that resurrects on the next restart -- noisy but not an exposure the operator
// believes is gone. Deleting the record first would invert that into the unsafe
// direction (record gone, service still serving, nothing left to reconcile it).
func (liveExposeOps) Unexpose(_ context.Context, name string) (*UnexposeResult, error) {
	if name == "" {
		return nil, fmt.Errorf("unexpose requires a service name")
	}
	ref := getProvisionedServiceGateway()
	if ref == nil {
		return nil, fmt.Errorf("no in-process gateway (unexpose requires the node gateway to be running)")
	}

	wasExposed := ref.gw.Unexpose(name)

	// Delete the durable record even when nothing was live: a record can outlive
	// its route (restored for a port that no longer listens, or written by an
	// older build), and revoke must be able to clear that too.
	if err := config.DeleteExposure(platform.ConfigDir(), name); err != nil {
		return nil, fmt.Errorf("exposure %q is no longer served, but its saved record could not be removed "+
			"(it will return on the next restart): %w", name, err)
	}
	return &UnexposeResult{Name: name, WasExposed: wasExposed}, nil
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

		if r.Path != "" {
			// Directory source (#943). The persisted Path is already the
			// resolved, workspace-confined root Expose() validated at write
			// time, so it is re-wired verbatim -- no re-validation against a
			// (possibly different-at-restore-time) workspace dir, matching the
			// port source's own "re-wire verbatim" contract below.
			if err := gw.ExposeDir(r.Name, r.Path, policy); err != nil {
				// A record the gateway rejects (unknown visibility, bad name, or
				// the directory no longer exists after a reboot) is data we
				// cannot honor -- skip it loudly rather than dropping the whole
				// set.
				Log("warning: skipping persisted exposure %q: %v", r.Name, err)
				continue
			}
			restored++
			continue
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
