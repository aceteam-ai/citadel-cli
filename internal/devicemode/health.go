// Pure self-heal decision logic for the device daemon (#5959).
//
// Kept free of I/O so the exact conditions under which a device reenrolls —
// the load-bearing behavior of device mode — are unit-testable without a
// tailscale daemon or a live mesh.
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

	// LeafWarnThreshold: how close to leaf expiry the daemon starts warning.
	// There is no leaf-renewal endpoint yet (see #5959 follow-ups), and the
	// reenroll terminator rejects EXPIRED client certs at the TLS handshake,
	// so an expiring leaf needs a fresh interactive enrollment before
	// not_after — the warning is the operator's runway.
	LeafWarnThreshold = 30 * 24 * time.Hour
)

// Decision is what the daemon should do this tick.
type Decision struct {
	// Reenroll: present the leaf to the reenroll service for a fresh authkey
	// and re-run tailscale up.
	Reenroll bool
	// Reason is a human-readable explanation for the log line.
	Reason string
	// LeafExpiresSoon: warn the operator to re-enroll interactively.
	LeafExpiresSoon bool
	// LeafExpired: the leaf can no longer authenticate at the mTLS
	// terminator; self-heal is impossible and the device must re-enroll
	// interactively. Overrides Reenroll.
	LeafExpired bool
}

// Decide evaluates one daemon tick.
//
// backendState is tailscale's BackendState; keyExpiry is the node key expiry
// (nil = non-expiring); leafNotAfter is the fabric leaf's expiry.
func Decide(backendState string, keyExpiry *time.Time, leafNotAfter, now time.Time) Decision {
	d := Decision{}

	if !leafNotAfter.After(now) {
		// The nginx mTLS terminator refuses an expired client cert at the
		// handshake, so the grace window is unreachable from a device: past
		// not_after the only path back is interactive enrollment.
		d.LeafExpired = true
		d.Reason = fmt.Sprintf(
			"fabric identity certificate expired %s — run 'citadel device enroll' to re-enroll",
			leafNotAfter.Format(time.RFC3339),
		)
		return d
	}
	if leafNotAfter.Sub(now) < LeafWarnThreshold {
		d.LeafExpiresSoon = true
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

	d.Reason = "healthy"
	return d
}
