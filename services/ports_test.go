package services

import "testing"

// TestMeetingHostPortsRegistered pins the meeting media-stack module's two
// loopback host ports (aceteam-ai/citadel-cli#514): they must be registered,
// distinct from each other and from every other managed/reserved port, and above
// the apps auto-allocation range. The module compose lives in citadel-services
// (not the embedded ServiceMap), so this registry is the only thing that stops a
// future module from hardcoding over 8207/8208.
func TestMeetingHostPortsRegistered(t *testing.T) {
	if MeetingdHostPort == MeetingCDPHostPort {
		t.Fatalf("meetingd (%d) and CDP (%d) host ports must differ", MeetingdHostPort, MeetingCDPHostPort)
	}
	for name, want := range map[string]int{"meeting": MeetingdHostPort, "meeting-cdp": MeetingCDPHostPort} {
		got, ok := ServiceHostPorts[name]
		if !ok || got != want {
			t.Errorf("ServiceHostPorts[%q] = %d (present=%v), want %d", name, got, ok, want)
		}
		if _, ok := serviceHostPortEnv[name]; !ok {
			t.Errorf("serviceHostPortEnv is missing %q; HostPortEnv() will not inject its host port", name)
		}
	}
	for _, port := range []int{MeetingdHostPort, MeetingCDPHostPort} {
		if port >= AppsPortRangeStart && port <= AppsPortRangeEnd {
			t.Errorf("meeting host port %d sits inside the apps auto-allocation range %d-%d", port, AppsPortRangeStart, AppsPortRangeEnd)
		}
		if name, taken := ReservedCitadelPorts[port]; taken {
			t.Errorf("meeting host port %d collides with reserved citadel port %q", port, name)
		}
	}
	// Pairwise-unique against every other managed service.
	for svc, port := range ServiceHostPorts {
		if svc == "meeting" || svc == "meeting-cdp" {
			continue
		}
		if port == MeetingdHostPort || port == MeetingCDPHostPort {
			t.Errorf("meeting host port collides with managed service %q (%d)", svc, port)
		}
	}
	// HostPortEnv must emit both meeting vars so a compose that defers to them
	// resolves.
	env := HostPortEnv()
	wantVars := map[string]bool{
		EnvMeetingdHostPort + "=8207":   false,
		EnvMeetingCDPHostPort + "=8208": false,
	}
	for _, kv := range env {
		if _, ok := wantVars[kv]; ok {
			wantVars[kv] = true
		}
	}
	for kv, seen := range wantVars {
		if !seen {
			t.Errorf("HostPortEnv() did not emit %q", kv)
		}
	}
}

// TestGotenbergHostPortRegistered pins the gotenberg document-conversion
// module's host port (aceteam-ai/citadel-services#10, unblocking Sovereign
// Sign P2 / aceteam#5793): it must be registered, distinct from every other
// managed/reserved port, and above the apps auto-allocation range. Like
// claudecode and meeting, gotenberg's compose lives in citadel-services (not
// the embedded ServiceMap), so this registry -- and this test -- is the only
// thing that stops a future module from hardcoding over 8209. The union guard
// in internal/apps/hostport_collision_test.go does NOT cover this port: it
// only claims ports from the apps catalog and services.ServiceMap compose
// files, neither of which gotenberg is part of.
func TestGotenbergHostPortRegistered(t *testing.T) {
	got, ok := ServiceHostPorts["gotenberg"]
	if !ok || got != GotenbergHostPort {
		t.Errorf("ServiceHostPorts[%q] = %d (present=%v), want %d", "gotenberg", got, ok, GotenbergHostPort)
	}
	if _, ok := serviceHostPortEnv["gotenberg"]; !ok {
		t.Errorf("serviceHostPortEnv is missing %q; HostPortEnv() will not inject its host port", "gotenberg")
	}
	if GotenbergHostPort >= AppsPortRangeStart && GotenbergHostPort <= AppsPortRangeEnd {
		t.Errorf("gotenberg host port %d sits inside the apps auto-allocation range %d-%d", GotenbergHostPort, AppsPortRangeStart, AppsPortRangeEnd)
	}
	if name, taken := ReservedCitadelPorts[GotenbergHostPort]; taken {
		t.Errorf("gotenberg host port %d collides with reserved citadel port %q", GotenbergHostPort, name)
	}
	// Pairwise-unique against every other managed service.
	for svc, port := range ServiceHostPorts {
		if svc == "gotenberg" {
			continue
		}
		if port == GotenbergHostPort {
			t.Errorf("gotenberg host port collides with managed service %q (%d)", svc, port)
		}
	}
	// HostPortEnv must emit the gotenberg var so a compose that defers to it
	// resolves.
	env := HostPortEnv()
	want := EnvGotenbergHostPort + "=8209"
	found := false
	for _, kv := range env {
		if kv == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("HostPortEnv() did not emit %q", want)
	}
}

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
