package services

import "testing"

// TestReservedCitadelPortsPairwiseDistinct asserts that no two DISTINCT
// citadel-owned listeners are registered on the same port. Reserved ports are
// a set-to-avoid for modules/apps (covered by the apps collision guard), but
// nothing else proved citadel's own listeners avoid EACH OTHER — which is
// exactly how #504 happened: the mTLS control listener and the HTTPS gateway
// both defaulted to mesh :8443, the control listener won the bind, and the
// gateway silently degraded to LAN-only. The map keying by port already makes
// duplicates impossible to EXPRESS, so the load-bearing assertions here are
// membership: every citadel-owned listener port must be present as its own
// entry (an accidental re-merge onto an existing port would silently collapse
// two entries into one).
func TestReservedCitadelPortsPairwiseDistinct(t *testing.T) {
	wantEntries := map[int]string{
		GatewayPort:       "gateway/status-server",
		GatewayHTTPSPort:  "gateway-https",
		ControlMTLSPort:   "control-mtls",
		TEIEmbeddingPort:  "tei-embeddings",
		VNCWebsockifyPort: "vnc-websockify",
		VNCPort:           "vnc-rfb",
		DeskstreamPort:    "deskstream-h264",
		TerminalPort:      "terminal-server",
		LiveKitWSPort:     "livekit-signaling",
		LiveKitICETCPPort: "livekit-ice-tcp",
		LiveKitUDPMuxPort: "livekit-udp-mux",
	}
	if len(ReservedCitadelPorts) != len(wantEntries) {
		t.Errorf("ReservedCitadelPorts has %d entries, want %d — a port constant collision collapses two listeners onto one port (see #504)",
			len(ReservedCitadelPorts), len(wantEntries))
	}
	for port, name := range wantEntries {
		if got, ok := ReservedCitadelPorts[port]; !ok || got != name {
			t.Errorf("ReservedCitadelPorts[%d] = %q (present=%v), want %q", port, got, ok, name)
		}
	}
}

// TestControlMTLSPortAvoidsKnownSurfaces pins the #504 fix: the control
// listener default must not sit on the HTTPS gateway port (both bind the mesh
// IP), and must stay clear of the apps auto-allocation range and the module
// registry so nothing else can be handed it.
func TestControlMTLSPortAvoidsKnownSurfaces(t *testing.T) {
	if ControlMTLSPort == GatewayHTTPSPort {
		t.Fatalf("ControlMTLSPort (%d) must differ from GatewayHTTPSPort (%d); sharing it silently kills mesh /vnc, /terminal, /modules/* (#504)",
			ControlMTLSPort, GatewayHTTPSPort)
	}
	if ControlMTLSPort >= AppsPortRangeStart && ControlMTLSPort <= AppsPortRangeEnd {
		t.Errorf("ControlMTLSPort (%d) sits inside the apps auto-allocation range %d-%d",
			ControlMTLSPort, AppsPortRangeStart, AppsPortRangeEnd)
	}
	for svc, port := range ServiceHostPorts {
		if port == ControlMTLSPort {
			t.Errorf("ControlMTLSPort (%d) collides with managed service %q", ControlMTLSPort, svc)
		}
	}
}
