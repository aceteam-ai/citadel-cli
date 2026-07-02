// cmd/keyrenew.go
// Background node-key renewal loop for the self-healing fabric node identity
// baseline (epic #4583, citadel side of backend PR #4584).
//
// While a node is healthy and online, this loop periodically inspects its
// Headscale node-key expiry and, when the key is approaching expiry, refreshes
// it IN PLACE using the node's own device token — never the offline
// authkey-mint recovery path. Keeping the key fresh while online means the
// session never lapses, so an ordinary restart re-establishes through the
// existing no-authkey reconnect and the 403 authkey-mint path is never reached.
package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
)

// keyRenewCheckInterval is how often the loop inspects key-expiry health. Key
// lifetimes are measured in days and the renewal threshold is 24h, so an hourly
// check is far more frequent than needed — cheap insurance that a node briefly
// offline around its threshold still renews on a subsequent tick.
const keyRenewCheckInterval = time.Hour

// keyRenewEnvInterval overrides the check interval (in seconds) and doubles as
// the opt-out switch: a non-positive value disables the loop entirely, matching
// the footprint sampler's gating convention (CITADEL_FOOTPRINT_INTERVAL<=0).
const keyRenewEnvInterval = "CITADEL_KEY_RENEW_INTERVAL"

// keyRenewIntervalFromEnv resolves the check interval and whether the loop is
// enabled. Unset uses the default; a positive value overrides; a non-positive
// value disables.
func keyRenewIntervalFromEnv() (time.Duration, bool) {
	raw := os.Getenv(keyRenewEnvInterval)
	if raw == "" {
		return keyRenewCheckInterval, true
	}
	var secs int
	if _, err := fmt.Sscanf(raw, "%d", &secs); err != nil {
		return keyRenewCheckInterval, true
	}
	if secs <= 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

// startNodeKeyRenewer launches the background node-key renewal loop unless it is
// disabled via --no-key-renew or a non-positive CITADEL_KEY_RENEW_INTERVAL. It
// no-ops when the node is not in API mode (no device token to fetch a fresh
// authkey with) since renewal has no admin-free path without it.
func startNodeKeyRenewer(ctx context.Context, deviceConfig *DeviceConfig) {
	if workNoKeyRenew {
		Debug("node-key renewer disabled (--no-key-renew)")
		return
	}
	interval, enabled := keyRenewIntervalFromEnv()
	if !enabled {
		Debug("node-key renewer disabled (%s<=0)", keyRenewEnvInterval)
		return
	}
	if deviceConfig == nil || deviceConfig.DeviceAPIToken == "" {
		Debug("node-key renewer disabled (no device token)")
		return
	}

	apiBaseURL := deviceConfig.APIBaseURL
	if apiBaseURL == "" {
		apiBaseURL = authServiceURL
	}

	go runNodeKeyRenewer(ctx, deviceConfig.DeviceAPIToken, apiBaseURL, interval)
	fmt.Printf("   - Node-key renewer: every %s (renews within %s of expiry)\n",
		interval, network.DefaultRenewThreshold)
}

// runNodeKeyRenewer is the loop body: on each tick, inspect key health and renew
// in place if the key is approaching (or past) expiry. Extracted so the tick
// logic is straightforward and the enable/gating decisions live in the caller.
func runNodeKeyRenewer(ctx context.Context, deviceToken, apiBaseURL string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkAndRenewNodeKey(ctx, deviceToken, apiBaseURL)
		}
	}
}

// checkAndRenewNodeKey performs a single inspect-and-maybe-renew cycle. It is a
// no-op unless the key is within the renewal threshold of expiry (or already
// expired). Failures are logged and swallowed — a transient renewal miss is
// recoverable on the next tick, and a hard failure (e.g. an old device token
// lacking the authkey scope) must not crash the node.
func checkAndRenewNodeKey(ctx context.Context, deviceToken, apiBaseURL string) {
	if !network.IsGlobalConnected() {
		// Not online — the reconnect/recovery path owns re-establishing the
		// session; there is no live key to renew in place.
		return
	}

	health, err := network.InspectGlobalNodeKey(ctx)
	if err != nil {
		Debug("node-key health check failed: %v", err)
		return
	}
	if health == nil {
		return
	}

	switch health.Status {
	case network.KeyNoExpiry, network.KeyHealthy:
		if health.Expiry != nil {
			Debug("node key healthy (expires %s)", health.Expiry.Format(time.RFC3339))
		} else {
			Debug("node key healthy (no expiry set)")
		}
		return
	case network.KeyRenewSoon, network.KeyExpired:
		when := "soon"
		if health.Expiry != nil {
			when = health.Expiry.Format(time.RFC3339)
		}
		Log("node key %s (expiry=%s); renewing in place...", health.Status, when)
	}

	// Fetch a fresh authkey using the node's own device token, then re-authorize
	// the running tsnet server in place (preserves IP + VPN listeners).
	fetchCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	freshKey, err := network.FetchFreshAuthkey(fetchCtx, apiBaseURL, deviceToken)
	cancel()
	if err != nil {
		// The 403 landmine: an OLD device token minted before the
		// device_authkey:write scope was granted (aceteam #4432) cannot fetch a
		// fresh authkey. The node is not broken — a persistent (non-ephemeral,
		// backend PR #4584) node still self-heals via the no-authkey reconnect on
		// restart — but it cannot renew online. Surface the exact remedy.
		if network.IsAuthkeyScopeError(err) {
			Log("node-key renewal blocked: device token predates the authkey scope; "+
				"re-run 'citadel init' to enable online self-renewal (%v)", err)
			fmt.Fprintln(os.Stderr, "   - Warning: node-key auto-renewal is disabled for this node.")
			fmt.Fprintln(os.Stderr, "     Its device token predates the self-renewal scope. Re-run "+
				"'citadel init' to enable it. The node still reconnects on restart.")
			return
		}
		Log("node-key renewal: could not fetch fresh authkey: %v", err)
		return
	}

	if err := network.RenewGlobalNodeKey(ctx, freshKey); err != nil {
		Log("node-key renewal: in-place reauth failed: %v", err)
		return
	}
	// Start() is async: it applies the fresh authkey to the running state
	// machine, which re-authorizes the node key without dropping the connection
	// (IP + VPN listeners preserved). If the key were somehow rejected, the next
	// tick still sees KeyRenewSoon and retries, so a single miss is self-healing.
	Log("node-key in-place reauth triggered with fresh authkey (IP + listeners preserved)")
	fmt.Println("   - Node key refreshed before expiry (session preserved)")
}
