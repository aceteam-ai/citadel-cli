package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/network"
)

// seamCounts tracks how many times each churn-relevant seam was invoked.
type seamCounts struct {
	fetchKey   int
	reconnect  int
	connect    int
	clearState int
	reclaim    int
}

// installRecoverSeams swaps recoverStaleVPN's seams for hermetic stubs that
// never touch real network state (on a live node the real ClearState()/Connect()
// would wipe this box's identity, issue #1235), restoring them on cleanup.
// reconnectOK/reconnectErr drive Attempt 1; connectErr drives the churn connect.
func installRecoverSeams(t *testing.T, reconnectOK bool, reconnectErr, connectErr error) *seamCounts {
	t.Helper()
	c := &seamCounts{}

	origFetch := fetchFreshAuthkeyFn
	origReconnect := reconnectWithAuthKeyFn
	origConnect := connectFn
	origClear := clearStateFn
	origStateDir := getStateDirFn
	origHasState := hasStateFn
	origResolve := resolveControlURLFn
	origReclaim := reclaimStaleNodeFn
	t.Cleanup(func() {
		fetchFreshAuthkeyFn = origFetch
		reconnectWithAuthKeyFn = origReconnect
		connectFn = origConnect
		clearStateFn = origClear
		getStateDirFn = origStateDir
		hasStateFn = origHasState
		resolveControlURLFn = origResolve
		reclaimStaleNodeFn = origReclaim
	})

	fetchFreshAuthkeyFn = func(context.Context, string, string) (string, error) {
		c.fetchKey++
		return "fake-authkey", nil
	}
	reconnectWithAuthKeyFn = func(context.Context, string) (bool, error) {
		c.reconnect++
		return reconnectOK, reconnectErr
	}
	connectFn = func(context.Context, network.ServerConfig) (*network.NetworkServer, error) {
		c.connect++
		return nil, connectErr
	}
	clearStateFn = func() error {
		c.clearState++
		return nil
	}
	getStateDirFn = func() string { return "/tmp/fake-citadel-state-DO-NOT-USE" }
	hasStateFn = func() bool { return true }
	resolveControlURLFn = func() string { return "http://fake-control" }
	reclaimStaleNodeFn = func(string, string) { c.reclaim++ }

	return c
}

func testRecoverDeviceConfig() *DeviceConfig {
	return &DeviceConfig{DeviceAPIToken: "tok", APIBaseURL: "http://api.example"}
}

// An unattended recovery (citadel work / egress / ingress / control-center,
// plus a plain `citadel reconnect`) must never clear state or re-register when
// the IP-preserving reconnect fails: it refuses and leaves the persisted
// identity intact so a later reattach recovers the SAME node (#1235).
func TestRecoverStaleVPN_UnattendedRefusesChurnOnAttempt1Failure(t *testing.T) {
	c := installRecoverSeams(t, false, errors.New("attempt1 failed"), nil)
	res := recoverStaleVPN(context.Background(), testRecoverDeviceConfig(), "node-a", "http://api.example", false)

	if res.Connected {
		t.Fatalf("expected refusal (not connected), got Connected=true")
	}
	if res.Err == nil {
		t.Fatalf("expected a refusal error, got nil")
	}
	if c.clearState != 0 {
		t.Errorf("unattended path must NOT ClearState; called %d times", c.clearState)
	}
	if c.connect != 0 {
		t.Errorf("unattended path must NOT re-register via Connect; called %d times", c.connect)
	}
	if c.reclaim != 0 {
		t.Errorf("unattended path must NOT reclaim the node; called %d times", c.reclaim)
	}
	if c.fetchKey != 1 {
		t.Errorf("expected exactly 1 authkey fetch (Attempt 1 only); got %d", c.fetchKey)
	}
}

// --force authorizes churn: an Attempt-1 failure falls through to reclaim +
// ClearState + a fresh Connect, and must fetch its OWN fresh authkey rather than
// reuse the possibly-spent single-use key (headscale #2434).
func TestRecoverStaleVPN_ForceChurnsWithOwnFreshKey(t *testing.T) {
	c := installRecoverSeams(t, false, errors.New("attempt1 failed"), nil)
	res := recoverStaleVPN(context.Background(), testRecoverDeviceConfig(), "node-a", "http://api.example", true)

	if !res.Connected {
		t.Fatalf("expected churn connect to succeed, got Connected=false (err=%v)", res.Err)
	}
	if res.IPPreserved {
		t.Errorf("churn path must report IPPreserved=false")
	}
	if c.clearState != 1 {
		t.Errorf("force path must ClearState once; got %d", c.clearState)
	}
	if c.connect != 1 {
		t.Errorf("force path must Connect once; got %d", c.connect)
	}
	if c.reclaim != 1 {
		t.Errorf("force path must reclaim once; got %d", c.reclaim)
	}
	if c.fetchKey != 2 {
		t.Errorf("force path must fetch a SECOND fresh authkey for the churn connect; got %d fetches", c.fetchKey)
	}
}

// A successful IP-preserving reconnect never churns, regardless of allowChurn.
func TestRecoverStaleVPN_Attempt1SuccessNeverChurns(t *testing.T) {
	for _, allowChurn := range []bool{false, true} {
		name := "unattended"
		if allowChurn {
			name = "force"
		}
		t.Run(name, func(t *testing.T) {
			c := installRecoverSeams(t, true, nil, nil)
			res := recoverStaleVPN(context.Background(), testRecoverDeviceConfig(), "node-a", "http://api.example", allowChurn)
			if !res.Connected || !res.IPPreserved {
				t.Fatalf("expected Connected+IPPreserved, got %+v", res)
			}
			if c.clearState != 0 || c.connect != 0 || c.reclaim != 0 {
				t.Errorf("Attempt-1 success must not churn (clear=%d connect=%d reclaim=%d)", c.clearState, c.connect, c.reclaim)
			}
		})
	}
}
