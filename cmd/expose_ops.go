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
	"sort"
	"sync"
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

// exposeOpsMu serializes every liveExposeOps operation (issue #944 design doc
// §5.4). liveExposeOps has TWO concurrent entry points inside one process — the
// worker lane (EXPOSE_SET/UNEXPOSE/EXPOSE_LIST jobs) and the /agent/expose +
// /agent/unexpose HTTP control handlers serving the local CLI — and
// SaveExposure/DeleteExposure are unlocked load-modify-writes of
// exposures.json. That race used to lose at worst one record; now that Expose
// computes an epoch via its own read-modify-write (resolveEffectiveEpoch,
// below), two concurrent calls could both read the same base and settle on the
// same effective epoch, silently defeating a `rotate` revocation.
//
// Deliberately NOT lane membership (serializedLaneJobTypes,
// internal/worker/deadline.go): a lane only serializes JOBS against each
// other — it has no visibility into the HTTP control endpoints, so lane
// membership alone would leave the CLI-vs-job race open. A plain mutex here
// covers both entry points with one lock. Held across the WHOLE op (epoch
// resolution + gateway programming + persistence) in Expose/Unexpose, and
// across the read in List for a consistent snapshot. These calls are
// milliseconds (map writes + one small file), so holding a mutex across them
// is not a lane-blocking concern.
var exposeOpsMu sync.Mutex

// resolveEffectiveEpoch computes the node-owned effective epoch for an
// EXPOSE_SET call (issue #944 design doc §5.3): the caller expresses INTENT
// (a fast-forward hint via reqEpoch, or an explicit revoke via rotate), the
// node is the AUTHORITY on the resulting value. base is the highest epoch this
// name has ever lived at (see the Expose call site for how base itself is
// derived from the durable record + the high-water store). Pure so it is
// tested without a gateway.
//
//   - A plain re-expose (rotate=false) never decreases the epoch (max(base,
//     reqEpoch)) and never increases it beyond a caller's own fast-forward —
//     so a blind, stateless caller sending the wire default (reqEpoch=1)
//     against a name already living at a higher epoch preserves it: no
//     outstanding link is revoked.
//   - rotate=true is the explicit revoke-all verb: it strictly increases the
//     epoch past base (and past any fast-forward the caller also sent), so
//     every previously issued token for this name stops verifying.
func resolveEffectiveEpoch(base, reqEpoch int, rotate bool) int {
	effective := base
	if reqEpoch > effective {
		effective = reqEpoch
	}
	if rotate {
		effective++
	}
	return effective
}

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
//
// Epoch custody (issue #944 design doc §5.3): the node, not the caller, owns
// the resulting epoch. base is the highest epoch this name has EVER lived at —
// a live durable record's own TokenEpoch when one exists, or one past this
// name's high-water mark when it does not (fresh, or the record was deleted by
// a prior UNEXPOSE). The high-water store outlives DeleteExposure, which is
// what stops an unexpose->re-expose sequence from resurrecting a revoked
// token (the exact hole a naive "reject a lower epoch" guard has once UNEXPOSE
// makes the durable record remotely deletable — see the design doc §5.2).
func (liveExposeOps) Expose(_ context.Context, req worker.ExposeRequest) (*worker.ExposeResult, error) {
	exposeOpsMu.Lock()
	defer exposeOpsMu.Unlock()

	ref := getProvisionedServiceGateway()
	if ref == nil {
		return nil, fmt.Errorf("no in-process gateway (expose requires the node gateway to be running)")
	}

	configDir := platform.ConfigDir()
	// The high-water store is a FLOOR, not just an absent-record fallback
	// (#945 review). A best-effort SaveExposure failure (or a legacy/
	// hand-edited record) can leave a STALE, lower-epoch record on disk while
	// the high-water store already records a higher, already-minted epoch. If
	// the record shadowed high-water, a blind re-expose would program the live
	// policy back DOWN to the stale epoch and resurrect a token the operator
	// explicitly rotated away. Flooring the record to high-water closes that,
	// and subsumes the legacy TokenEpoch==0 case (hw > 0 lifts base off 0).
	hw := config.ExposeEpochHighWater(configDir, req.Name)
	base := 1
	if existing := config.FindExposure(configDir, req.Name); existing != nil {
		base = existing.TokenEpoch
		if hw > base {
			base = hw
		}
	} else if hw > 0 {
		base = hw + 1
	}
	effective := resolveEffectiveEpoch(base, req.Epoch, req.Rotate)
	if err := config.SaveExposeEpochHighWater(configDir, req.Name, effective); err != nil {
		// Non-fatal (mirrors the persistence-is-best-effort posture below): the
		// exposure still programs correctly at `effective` for this process's
		// lifetime, the loss is scoped to future high-water memory surviving a
		// restart. Logged so it is visible, not returned, for the same "the
		// exposure IS live, failing the call would be a lie" reasoning as the
		// SaveExposure failure below.
		Log("warning: exposure %q's epoch high-water was not persisted: %v", req.Name, err)
	}

	policy := &gateway.ExposePolicy{
		Visibility: gateway.Visibility(req.Visibility),
		Creator:    req.Creator,
		TokenEpoch: effective,
	}

	rec := config.ExposureRecord{
		Name:       req.Name,
		Visibility: req.Visibility,
		Creator:    req.Creator,
		TokenEpoch: effective,
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
	if err := config.SaveExposure(configDir, rec); err != nil {
		Log("warning: exposure %q is live but was not persisted (it will not survive a restart): %v", req.Name, err)
	}

	res := &worker.ExposeResult{URL: exposeMeshURL(req.Name), Epoch: effective}

	if policy.Visibility == gateway.VisibilityLink {
		key, err := config.LoadOrCreateExposeSigningKey(configDir)
		if err != nil {
			return nil, fmt.Errorf("load link signing key: %w", err)
		}
		ttl := time.Duration(req.TTLSeconds) * time.Second
		if ttl <= 0 {
			ttl = defaultLinkTTL
		}
		exp := time.Now().Add(ttl)
		res.Token = gateway.MintLinkToken(key, req.Name, effective, exp)
		res.ExpiresAt = exp.UTC().Format(time.RFC3339)
	}
	return res, nil
}

// UnexposeResult is an alias of the worker-side type (worker.UnexposeResult):
// cmd already imports worker (never the reverse — see the worker.ExposeOps doc
// comment), so this keeps every existing reference to cmd.UnexposeResult
// working while the actual struct lives in the leaf package the UNEXPOSE
// handler also uses.
type UnexposeResult = worker.UnexposeResult

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
//
// Deliberately does NOT touch the epoch high-water store (issue #944 design
// doc §4.2): UNEXPOSE's job is done the moment the live route and the durable
// record are gone (old tokens die because the gateway 404s an unregistered
// name). What stops a LATER re-expose from resurrecting an old token is
// Expose's own high-water read, not anything this function does.
func (liveExposeOps) Unexpose(_ context.Context, name string) (*worker.UnexposeResult, error) {
	exposeOpsMu.Lock()
	defer exposeOpsMu.Unlock()

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
	return &worker.UnexposeResult{Name: name, WasExposed: wasExposed}, nil
}

// List returns the durable exposure inventory merged with the gateway's live
// policy names (issue #944, design doc §3.2). The durable set
// (config.LoadExposures) is the authority — it is what survives restarts —
// but each row also carries a `Live` bit read from the gateway's in-memory
// policy table, and any LIVE-ONLY exposure (present in the gateway but with no
// durable record — e.g. SaveExposure's best-effort write failed, or the
// record predates a field this build now requires) is surfaced separately
// rather than silently omitted. Locked for a consistent snapshot against a
// concurrent Expose/Unexpose, not because reading is itself expensive.
func (liveExposeOps) List(_ context.Context) (*worker.ExposeListResult, error) {
	exposeOpsMu.Lock()
	defer exposeOpsMu.Unlock()

	recs, err := config.LoadExposures(platform.ConfigDir())
	if err != nil {
		return nil, fmt.Errorf("load exposures: %w", err)
	}

	liveNames := map[string]bool{}
	if ref := getProvisionedServiceGateway(); ref != nil {
		for _, n := range ref.gw.ExposureNames() {
			liveNames[n] = true
		}
	}

	out := &worker.ExposeListResult{Exposures: make([]worker.ExposureInfo, 0, len(recs))}
	seen := make(map[string]bool, len(recs))
	for _, r := range recs {
		seen[r.Name] = true
		out.Exposures = append(out.Exposures, worker.ExposureInfo{
			Name:       r.Name,
			Port:       r.Port,
			Path:       r.Path,
			Visibility: r.Visibility,
			Creator:    r.Creator,
			Epoch:      r.TokenEpoch,
			CreatedAt:  r.CreatedAt,
			Live:       liveNames[r.Name],
		})
	}
	for n := range liveNames {
		if !seen[n] {
			out.LiveOnly = append(out.LiveOnly, n)
		}
	}
	sort.Strings(out.LiveOnly)
	return out, nil
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
		// Floor the restored epoch to the high-water store (#945 review). A
		// best-effort SaveExposure failure can leave a stale, lower-epoch
		// record on disk after a rotate that already advanced high-water; a
		// routine restart (auto-update, self-heal, reboot) would otherwise
		// program the live policy straight from that stale r.TokenEpoch and
		// resurrect the rotated-away token with no re-expose at all. The
		// high-water store is never decremented, so this only ever raises the
		// restored epoch, never lowers it.
		epoch := r.TokenEpoch
		if hw := config.ExposeEpochHighWater(platform.ConfigDir(), r.Name); hw > epoch {
			epoch = hw
		}
		policy := &gateway.ExposePolicy{
			Visibility: gateway.Visibility(r.Visibility),
			Creator:    r.Creator,
			TokenEpoch: epoch,
		}

		if r.Path != "" {
			// Directory source (#943/#949). The persisted Path was the
			// resolved, workspace-confined root Expose() validated at WRITE
			// time -- but the workspace boundary itself is not durable: an
			// operator can narrow CITADEL_WORKSPACE (or --workspace) between
			// then and this restart, so a previously-authorized directory
			// could now sit outside the CURRENT workspace. Re-run the same
			// jobs.ValidatePath boundary Expose() used at write time before
			// re-wiring it, and skip loudly (never fall back to serving it
			// anyway) when it no longer resolves inside the workspace --
			// defense-in-depth: the directory is still symlink-confined to
			// its own resolved root regardless (resolveConfinedTarget, every
			// request), and reaching this state requires an authorized
			// operator reconfiguration, not an attacker action.
			if _, err := jobs.ValidatePath(resolveWorkspaceDir(), r.Path); err != nil {
				Log("warning: skipping persisted exposure %q: path %q no longer resolves inside the node workspace: %v", r.Name, r.Path, err)
				continue
			}
			// ExposeDir independently re-resolves and re-validates r.Path
			// (resolveConfinedRoot: absolute, EvalSymlinks, must still be an
			// existing directory) rather than trusting the stored string, so
			// this also catches the directory having been deleted or moved
			// since it was persisted.
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
