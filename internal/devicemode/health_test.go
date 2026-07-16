package devicemode

import (
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

func ptr(t time.Time) *time.Time { return &t }

func TestDecideHealthy(t *testing.T) {
	leafNotAfter := testNow.Add(200 * 24 * time.Hour)
	keyExpiry := ptr(testNow.Add(90 * 24 * time.Hour))

	d := Decide("Running", keyExpiry, leafNotAfter, testNow)
	if d.Reenroll || d.LeafExpired || d.LeafExpiresSoon {
		t.Fatalf("expected healthy no-op, got %+v", d)
	}
}

func TestDecideNoKeyExpiry(t *testing.T) {
	leafNotAfter := testNow.Add(200 * 24 * time.Hour)
	d := Decide("Running", nil, leafNotAfter, testNow)
	if d.Reenroll {
		t.Fatalf("non-expiring key must not trigger reenroll: %+v", d)
	}
}

func TestDecideNeedsLogin(t *testing.T) {
	leafNotAfter := testNow.Add(200 * 24 * time.Hour)
	d := Decide("NeedsLogin", nil, leafNotAfter, testNow)
	if !d.Reenroll {
		t.Fatalf("NeedsLogin must trigger reenroll: %+v", d)
	}
}

func TestDecideStoppedRespectsOperator(t *testing.T) {
	// `tailscale down` is a deliberate operator action; a fresh authkey would
	// not restart the engine and we must not fight the user.
	leafNotAfter := testNow.Add(200 * 24 * time.Hour)
	d := Decide("Stopped", nil, leafNotAfter, testNow)
	if d.Reenroll {
		t.Fatalf("Stopped must not trigger reenroll: %+v", d)
	}
}

func TestDecideKeyExpiringSoon(t *testing.T) {
	leafNotAfter := testNow.Add(200 * 24 * time.Hour)
	keyExpiry := ptr(testNow.Add(6 * time.Hour)) // < 24h threshold

	d := Decide("Running", keyExpiry, leafNotAfter, testNow)
	if !d.Reenroll {
		t.Fatalf("key inside renew threshold must trigger reenroll: %+v", d)
	}
}

func TestDecideKeyAlreadyExpired(t *testing.T) {
	leafNotAfter := testNow.Add(200 * 24 * time.Hour)
	keyExpiry := ptr(testNow.Add(-time.Hour))

	d := Decide("Running", keyExpiry, leafNotAfter, testNow)
	if !d.Reenroll {
		t.Fatalf("expired key must trigger reenroll: %+v", d)
	}
}

func TestDecideLeafExpiringSoonWarns(t *testing.T) {
	leafNotAfter := testNow.Add(10 * 24 * time.Hour) // < 30d warn threshold
	d := Decide("Running", nil, leafNotAfter, testNow)
	if !d.LeafExpiresSoon {
		t.Fatalf("leaf inside warn threshold must warn: %+v", d)
	}
	if d.Reenroll || d.LeafExpired {
		t.Fatalf("warn-only case must not reenroll/expire: %+v", d)
	}
}

func TestDecideLeafExpiredBlocksSelfHeal(t *testing.T) {
	// The mTLS terminator rejects expired client certs at the handshake, so
	// past not_after the ONLY path is interactive re-enrollment — even when
	// the session is broken.
	leafNotAfter := testNow.Add(-time.Hour)
	d := Decide("NeedsLogin", nil, leafNotAfter, testNow)
	if !d.LeafExpired {
		t.Fatalf("expired leaf must be flagged: %+v", d)
	}
	if d.Reenroll {
		t.Fatalf("expired leaf must suppress reenroll: %+v", d)
	}
	if !strings.Contains(d.Reason, "citadel device enroll") {
		t.Fatalf("expired-leaf reason must point at the remedy, got %q", d.Reason)
	}
}
