package heartbeat

import (
	"context"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

func TestAPIHeartbeatCarriesLiveControlCertificateAndClearsStoppedListener(t *testing.T) {
	stub := &redisAPIStub{}
	pub, _ := newTestAPIPublisher(t, stub)
	var current *status.ControlTLSReport
	pub.SetControlTLSProvider(func() *status.ControlTLSReport { return current })
	for _, pem := range []string{"", "loaded-first-cert", "loaded-second-cert", ""} {
		current = nil
		if pem != "" {
			current = &status.ControlTLSReport{CertPEM: pem}
		}
		if err := pub.publishStatus(context.Background()); err != nil {
			t.Fatal(err)
		}
		landed := stub.landed()
		got := landed[len(landed)-1].ControlTLS
		if pem == "" {
			if got != nil {
				t.Fatal("not-serving listener must omit evidence")
			}
		} else if got == nil || got.CertPEM != pem {
			t.Fatal("durable API payload dropped or retained stale loaded evidence")
		}
	}
}

func TestLegacyRedisHeartbeatOmitsUnauthenticatedControlEvidence(t *testing.T) {
	pub, mr, _ := newTestRedisPublisher(t)
	if err := pub.publishStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(streamEntries(t, mr)[0]["payload"], "control_tls") {
		t.Fatal("unsigned legacy Redis must not publish control certificate evidence")
	}
}
