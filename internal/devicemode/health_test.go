package devicemode

import (
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

func ptr(t time.Time) *time.Time { return &t }

func decide(backendState string, keyExpiry *time.Time, leafNotAfter time.Time) Decision {
	return Decide(backendState, keyExpiry, leafNotAfter, testNow, DefaultLeafRenewThreshold)
}

func TestDecideHealthy(t *testing.T) {
	leafNotAfter := testNow.Add(200 * 24 * time.Hour)
	keyExpiry := ptr(testNow.Add(90 * 24 * time.Hour))

	d := decide("Running", keyExpiry, leafNotAfter)
	if d.Reenroll || d.RenewLeaf || d.LeafExpired || d.LeafExpiresSoon {
		t.Fatalf("expected healthy no-op, got %+v", d)
	}
}

func TestDecideNoKeyExpiry(t *testing.T) {
	leafNotAfter := testNow.Add(200 * 24 * time.Hour)
	d := decide("Running", nil, leafNotAfter)
	if d.Reenroll {
		t.Fatalf("non-expiring key must not trigger reenroll: %+v", d)
	}
}

func TestDecideNeedsLogin(t *testing.T) {
	leafNotAfter := testNow.Add(200 * 24 * time.Hour)
	d := decide("NeedsLogin", nil, leafNotAfter)
	if !d.Reenroll {
		t.Fatalf("NeedsLogin must trigger reenroll: %+v", d)
	}
}

func TestDecideStoppedRespectsOperator(t *testing.T) {
	// `tailscale down` is a deliberate operator action; a fresh authkey would
	// not restart the engine and we must not fight the user.
	leafNotAfter := testNow.Add(200 * 24 * time.Hour)
	d := decide("Stopped", nil, leafNotAfter)
	if d.Reenroll {
		t.Fatalf("Stopped must not trigger reenroll: %+v", d)
	}
}

func TestDecideKeyExpiringSoon(t *testing.T) {
	leafNotAfter := testNow.Add(200 * 24 * time.Hour)
	keyExpiry := ptr(testNow.Add(6 * time.Hour)) // < 24h threshold

	d := decide("Running", keyExpiry, leafNotAfter)
	if !d.Reenroll {
		t.Fatalf("key inside renew threshold must trigger reenroll: %+v", d)
	}
}

func TestDecideKeyAlreadyExpired(t *testing.T) {
	leafNotAfter := testNow.Add(200 * 24 * time.Hour)
	keyExpiry := ptr(testNow.Add(-time.Hour))

	d := decide("Running", keyExpiry, leafNotAfter)
	if !d.Reenroll {
		t.Fatalf("expired key must trigger reenroll: %+v", d)
	}
}

func TestDecideLeafInsideRenewWindowRenews(t *testing.T) {
	leafNotAfter := testNow.Add(10 * 24 * time.Hour) // < 30d renew threshold
	d := decide("Running", nil, leafNotAfter)
	if !d.RenewLeaf {
		t.Fatalf("leaf inside renew window must trigger renewal: %+v", d)
	}
	if !d.LeafExpiresSoon {
		t.Fatalf("leaf inside renew window must warn: %+v", d)
	}
	if d.Reenroll || d.LeafExpired {
		t.Fatalf("renew-only case must not reenroll/expire: %+v", d)
	}
}

func TestDecideCustomRenewThreshold(t *testing.T) {
	leafNotAfter := testNow.Add(10 * 24 * time.Hour)
	// A 5d threshold leaves a 10d-remaining leaf alone...
	d := Decide("Running", nil, leafNotAfter, testNow, 5*24*time.Hour)
	if d.RenewLeaf {
		t.Fatalf("leaf outside custom threshold must not renew: %+v", d)
	}
	// ...and a 60d threshold renews it.
	d = Decide("Running", nil, leafNotAfter, testNow, 60*24*time.Hour)
	if !d.RenewLeaf {
		t.Fatalf("leaf inside custom threshold must renew: %+v", d)
	}
}

func TestDecideRenewAndReenrollCanCoincide(t *testing.T) {
	// A broken session and an expiring leaf in the same tick: fix both.
	leafNotAfter := testNow.Add(10 * 24 * time.Hour)
	d := decide("NeedsLogin", nil, leafNotAfter)
	if !d.Reenroll || !d.RenewLeaf {
		t.Fatalf("expected both reenroll and renew, got %+v", d)
	}
}

func TestDecideLeafExpiredInGraceStillSelfHeals(t *testing.T) {
	// Expired-but-in-grace: the reenroll service accepts the leaf for mesh
	// authkeys (nexus#37 made the grace window reachable through the
	// terminator), so session self-heal must still fire. Renewal must NOT:
	// it requires a currently-valid leaf.
	leafNotAfter := testNow.Add(-10 * 24 * time.Hour) // 10d expired, grace 60d
	d := decide("NeedsLogin", nil, leafNotAfter)
	if !d.LeafExpiredInGrace {
		t.Fatalf("expired-in-grace leaf must be flagged: %+v", d)
	}
	if !d.Reenroll {
		t.Fatalf("expired-in-grace leaf must still attempt session self-heal: %+v", d)
	}
	if d.RenewLeaf {
		t.Fatalf("expired leaf must never attempt renewal: %+v", d)
	}
	if d.LeafExpired {
		t.Fatalf("in-grace leaf is not terminally expired: %+v", d)
	}
}

func TestDecideLeafExpiredInGraceHealthySessionPointsAtRemedy(t *testing.T) {
	leafNotAfter := testNow.Add(-10 * 24 * time.Hour)
	d := decide("Running", nil, leafNotAfter)
	if d.Reenroll || d.RenewLeaf {
		t.Fatalf("healthy session must not reenroll/renew: %+v", d)
	}
	if !strings.Contains(d.Reason, "citadel device enroll") {
		t.Fatalf("in-grace reason must point at the remedy, got %q", d.Reason)
	}
}

func TestDecideLeafExpiredPastGraceBlocksSelfHeal(t *testing.T) {
	// Past not_after + grace nothing can authenticate — even a broken session
	// must not trigger a doomed reenroll loop.
	leafNotAfter := testNow.Add(-time.Duration(LeafGraceDays+10) * 24 * time.Hour)
	d := decide("NeedsLogin", nil, leafNotAfter)
	if !d.LeafExpired {
		t.Fatalf("past-grace leaf must be flagged terminal: %+v", d)
	}
	if d.Reenroll || d.RenewLeaf {
		t.Fatalf("past-grace leaf must suppress reenroll/renew: %+v", d)
	}
	if !strings.Contains(d.Reason, "citadel device enroll") {
		t.Fatalf("past-grace reason must point at the remedy, got %q", d.Reason)
	}
}
