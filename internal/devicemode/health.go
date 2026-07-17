// Pure self-heal decision logic for the device daemon (#5959).
//
// Kept free of I/O so the exact conditions under which a device reenrolls or
// renews its leaf — the load-bearing behavior of device mode — are
// unit-testable without a tailscale daemon or a live mesh.
package devicemode

import (
	"fmt"
	"time"
)

const (
	// KeyRenewThreshold: reenroll while the node key still has this much
	// life left, so the session never actually lapses. Mirrors the node-side
	// renewer's 24h threshold (internal/network/keyhealth.go).
	KeyRenewThreshold = 24 * time.Hour

	// DefaultLeafRenewThreshold: renew the fabric leaf while it still has
	// this much life left (env CITADEL_DEVICE_LEAF_RENEW_DAYS overrides).
	// Renewal is fully automatic (platform POST /api/fabric/ca/renew with a
	// CSR proof-of-possession) and requires a still-valid leaf, so the window
	// must be comfortably wider than the daemon's check interval and any
	// plausible laptop-in-a-drawer stretch.
	DefaultLeafRenewThreshold = 30 * 24 * time.Hour

	// LeafGraceDays mirrors the reenroll service's REENROLL_GRACE_DAYS
	// default: an expired-but-not-revoked leaf may still authenticate to the
	// reenroll service (for a mesh authkey only — NOT for leaf renewal) until
	// not_after + this many days. Past it, only interactive enrollment
	// remains. Kept as a client-side mirror so the daemon's messaging matches
	// what the service will actually do.
	LeafGraceDays = 60
)

// Decision is what the daemon should do this tick.
type Decision struct {
	// Reenroll: present the leaf to the reenroll service for a fresh authkey
	// and re-run tailscale up.
	Reenroll bool
	// RenewLeaf: the leaf is still valid but inside the renewal window —
	// obtain a successor leaf from the platform before it expires.
	RenewLeaf bool
	// Reason is a human-readable explanation for the log line.
	Reason string
	// LeafExpiresSoon: the leaf is inside the renewal window. If automatic
	// renewal keeps failing, the operator eventually needs to re-enroll
	// interactively before not_after.
	LeafExpiresSoon bool
	// LeafExpiredInGrace: the leaf is expired but within the reenroll grace
	// window — mesh self-heal (Reenroll) can still work, but the leaf itself
	// can no longer be renewed; re-enroll interactively to restore it.
	LeafExpiredInGrace bool
	// LeafExpired: the leaf is expired PAST the grace window; nothing
	// automatic remains and the device must re-enroll interactively.
	// Overrides Reenroll.
	LeafExpired bool
}

// Decide evaluates one daemon tick.
//
// backendState is tailscale's BackendState; keyExpiry is the node key expiry
// (nil = non-expiring); leafNotAfter is the fabric leaf's expiry;
// leafRenewThreshold is how much remaining leaf life triggers renewal
// (use DefaultLeafRenewThreshold unless overridden).
func Decide(
	backendState string,
	keyExpiry *time.Time,
	leafNotAfter, now time.Time,
	leafRenewThreshold time.Duration,
) Decision {
	d := Decision{}

	if !leafNotAfter.After(now) {
		graceDeadline := leafNotAfter.Add(LeafGraceDays * 24 * time.Hour)
		if now.After(graceDeadline) {
			// Past grace nothing can authenticate: not the reenroll service,
			// not renewal. Interactive enrollment is the only path back.
			d.LeafExpired = true
			d.Reason = fmt.Sprintf(
				"fabric identity certificate expired %s (past the %dd grace window) — run 'citadel device enroll' to re-enroll",
				leafNotAfter.Format(time.RFC3339), LeafGraceDays,
			)
			return d
		}
		// Expired but in grace: the reenroll service still accepts the leaf
		// for mesh authkeys (nexus#37 made this reachable through the
		// terminator), so session self-heal continues below. The leaf itself
		// cannot be renewed anymore — renewal requires a valid leaf — so keep
		// pointing the operator at interactive enrollment.
		d.LeafExpiredInGrace = true
	} else {
		if leafNotAfter.Sub(now) < leafRenewThreshold {
			d.RenewLeaf = true
			d.LeafExpiresSoon = true
		}
	}

	switch backendState {
	case "NeedsLogin", "NoState":
		d.Reenroll = true
		d.Reason = fmt.Sprintf("tailscale session needs login (state=%s)", backendState)
		return d
	case "Stopped":
		// Deliberate `tailscale down` — respect the operator's choice; a
		// fresh authkey would not start the engine anyway.
		d.Reason = "tailscale is stopped (operator action); not intervening"
		return d
	}

	if keyExpiry != nil && !keyExpiry.IsZero() {
		until := keyExpiry.Sub(now)
		if until <= 0 {
			d.Reenroll = true
			d.Reason = fmt.Sprintf("node key expired %s", keyExpiry.Format(time.RFC3339))
			return d
		}
		if until < KeyRenewThreshold {
			d.Reenroll = true
			d.Reason = fmt.Sprintf("node key expires in %s (< %s)", until.Round(time.Minute), KeyRenewThreshold)
			return d
		}
	}

	switch {
	case d.RenewLeaf:
		d.Reason = fmt.Sprintf(
			"identity certificate expires %s — renewing", leafNotAfter.Format("2006-01-02"))
	case d.LeafExpiredInGrace:
		d.Reason = fmt.Sprintf(
			"identity certificate expired %s but within the %dd grace window — mesh self-heal still works; re-run 'citadel device enroll' to restore the certificate",
			leafNotAfter.Format("2006-01-02"), LeafGraceDays,
		)
	default:
		d.Reason = "healthy"
	}
	return d
}
