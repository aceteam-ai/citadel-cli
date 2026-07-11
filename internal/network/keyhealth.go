// internal/network/keyhealth.go
// Node-key health inspection + in-place renewal for the self-healing fabric
// node identity baseline (epic #4583).
//
// Background: a Citadel node authenticates its tsnet/WireGuard session with a
// Headscale preauth-derived node key. tsnet does NOT proactively renew that key
// — it only re-authorizes when Start() is called with a fresh authkey (or via
// interactive login, which is useless headless). If Nexus mints keys with an
// expiry, a long-lived session's node key can EXPIRE while the node is online,
// dropping it off the mesh. The defense: while healthy and online, notice the
// approaching expiry and re-authorize IN PLACE using the node's own
// already-authenticated session, so the key never lapses and the offline 403
// authkey-mint path is never reached.
package network

import (
	"context"
	"fmt"
	"time"

	"tailscale.com/ipn"
)

// KeyStatus classifies the health of the node's Headscale key relative to its
// expiry time. It is the output of the pure keyRenewalDecision function so the
// decision logic can be tested without a live node.
type KeyStatus int

const (
	// KeyNoExpiry means the key has no expiry set (nil/zero KeyExpiry). This is
	// the durable-baseline healthy state — a non-expiring key never needs
	// proactive renewal, so the renewer treats it as a no-op.
	KeyNoExpiry KeyStatus = iota
	// KeyHealthy means the key has an expiry but it is comfortably in the
	// future (more than the renewal threshold away). No action needed.
	KeyHealthy
	// KeyRenewSoon means the key expires within the renewal threshold. The
	// renewer should refresh it now, while still online, before it lapses.
	KeyRenewSoon
	// KeyExpired means the key has already expired. The session is (or is about
	// to be) stale; an in-place renewal is still worth attempting before falling
	// through to the heavier reconnect/recovery path.
	KeyExpired
)

func (s KeyStatus) String() string {
	switch s {
	case KeyNoExpiry:
		return "no-expiry"
	case KeyHealthy:
		return "healthy"
	case KeyRenewSoon:
		return "renew-soon"
	case KeyExpired:
		return "expired"
	default:
		return "unknown"
	}
}

// DefaultRenewThreshold is how far ahead of expiry the renewer refreshes the
// node key. Headscale/Nexus preauth key lifetimes are typically measured in
// days; refreshing a day early leaves ample margin for a node that is briefly
// offline (asleep, network blip) to still renew on its next healthy tick
// without ever lapsing.
const DefaultRenewThreshold = 24 * time.Hour

// keyRenewalDecision classifies a key's health from its expiry time. It is a
// pure function (no I/O) so the branching — especially the nil/zero "no expiry"
// case — is unit-testable without a live node.
//
// A nil or zero keyExpiry means the key does not expire; that is the healthy
// durable-baseline state, NOT an "unknown, act now" state, so it maps to
// KeyNoExpiry (a no-op for the renewer).
func keyRenewalDecision(keyExpiry *time.Time, now time.Time, threshold time.Duration) KeyStatus {
	if keyExpiry == nil || keyExpiry.IsZero() {
		return KeyNoExpiry
	}
	if !now.Before(*keyExpiry) {
		return KeyExpired
	}
	if keyExpiry.Sub(now) <= threshold {
		return KeyRenewSoon
	}
	return KeyHealthy
}

// NodeKeyHealth holds the observed key-expiry state for the global server.
type NodeKeyHealth struct {
	// Status is the classification relative to the renewal threshold.
	Status KeyStatus
	// Expiry is the key's expiry time, if the control plane reported one.
	// Nil means the key does not expire (durable baseline).
	Expiry *time.Time
	// Expired mirrors tailscale's own Self.Expired flag (the control plane's
	// authoritative view), independent of our threshold arithmetic.
	Expired bool
}

// InspectGlobalNodeKey reads the global server's self status and classifies the
// node key's expiry health using the default renewal threshold. Returns a nil
// health (and no error) when not connected — there is no key to inspect.
func InspectGlobalNodeKey(ctx context.Context) (*NodeKeyHealth, error) {
	return InspectGlobalNodeKeyWithThreshold(ctx, DefaultRenewThreshold)
}

// InspectGlobalNodeKeyWithThreshold is InspectGlobalNodeKey with an explicit
// renewal threshold (used by the background loop and by tests).
func InspectGlobalNodeKeyWithThreshold(ctx context.Context, threshold time.Duration) (*NodeKeyHealth, error) {
	s := Global()
	if s == nil {
		return nil, nil
	}

	lc, err := s.LocalClient()
	if err != nil {
		return nil, fmt.Errorf("local client: %w", err)
	}

	status, err := lc.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	if status.Self == nil {
		// Netmap not populated yet (e.g. just after connect). Treat as
		// unknown-but-benign: no expiry data means nothing to renew right now.
		return &NodeKeyHealth{Status: KeyNoExpiry}, nil
	}

	expiry := status.Self.KeyExpiry
	return &NodeKeyHealth{
		Status:  keyRenewalDecision(expiry, time.Now(), threshold),
		Expiry:  expiry,
		Expired: status.Self.Expired,
	}, nil
}

// RenewGlobalNodeKey re-authorizes the RUNNING global tsnet server in place with
// a fresh authkey, without tearing down the connection. It calls the local
// backend's Start with the new AuthKey — the same mechanism as
// `tailscale up --auth-key` — so the machine key (and thus the node's IP) and
// all existing tsnet listeners (status/terminal/gateway bound at startup) are
// preserved. This is what makes online renewal safe versus the offline
// ReconnectWithAuthKey path, which spins up a second tsnet.Server and requires a
// disruptive Disconnect first.
//
// The caller supplies the fresh authkey (fetched via FetchFreshAuthkey using the
// node's own device token — no admin scope beyond what the node already holds).
func RenewGlobalNodeKey(ctx context.Context, authKey string) error {
	if authKey == "" {
		return fmt.Errorf("empty authkey")
	}
	s := Global()
	if s == nil {
		return fmt.Errorf("not connected to AceTeam Network")
	}
	lc, err := s.LocalClient()
	if err != nil {
		return fmt.Errorf("local client: %w", err)
	}
	// Start applies the new AuthKey to the already-running state machine,
	// re-authorizing the node key in place. UpdatePrefs is left nil so existing
	// prefs (and Persist, i.e. the machine key) are preserved.
	if err := lc.Start(ctx, ipn.Options{AuthKey: authKey}); err != nil {
		return fmt.Errorf("in-place reauth: %w", err)
	}
	return nil
}
