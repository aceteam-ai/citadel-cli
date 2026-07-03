package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGatewayRouteURL covers the pure mesh-URL builder: scheme selection and the
// off-mesh empty-IP case.
func TestGatewayRouteURL(t *testing.T) {
	tests := []struct {
		name   string
		useTLS bool
		ip     string
		port   int
		prefix string
		want   string
	}{
		{"https default", true, "100.64.0.7", 8443, "/whatsapp", "https://100.64.0.7:8443/whatsapp"},
		{"http no-tls", false, "100.64.0.7", 8443, "/whatsapp", "http://100.64.0.7:8443/whatsapp"},
		{"custom port", true, "100.64.0.9", 9443, "/whatsapp", "https://100.64.0.9:9443/whatsapp"},
		{"off-mesh empty ip", true, "", 8443, "/whatsapp", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := gatewayRouteURL(tc.useTLS, tc.ip, tc.port, tc.prefix)
			if got != tc.want {
				t.Errorf("gatewayRouteURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVerifyBridgeReachable_OK: a 200 from the (gateway-fronted) bridge root
// means the mesh path works.
func TestVerifyBridgeReachable_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			t.Errorf("probe path = %q, want /", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := verifyBridgeReachable(context.Background(), srv.URL); err != nil {
		t.Fatalf("verifyBridgeReachable() = %v, want nil for a 200 root", err)
	}
}

// TestVerifyBridgeReachable_502: the gateway returns 502 when the route has no
// live upstream -- the exact unreachable-bridge failure #447 guards against.
func TestVerifyBridgeReachable_502(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	err := verifyBridgeReachable(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected an unreachable error for a 502, got nil")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error = %q, want it to mention the 502", err.Error())
	}
}

// TestVerifyBridgeReachable_DialFail: nothing listening -> unreachable.
func TestVerifyBridgeReachable_DialFail(t *testing.T) {
	// A closed server address (started then immediately closed) reliably refuses.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	if err := verifyBridgeReachable(context.Background(), url); err == nil {
		t.Fatal("expected a dial error against a closed listener, got nil")
	}
}

// TestGatewayPortAndTLS_DefaultsWhenNoGateway verifies the CLI/TUI advertise the
// deterministic gateway URL (8443, TLS) when this process is not the one running
// the gateway.
func TestGatewayPortAndTLS_DefaultsWhenNoGateway(t *testing.T) {
	setProvisionedServiceGateway(nil, 0, false) // ensure cleared
	port, useTLS := gatewayPortAndTLS()
	if port != DefaultGatewayPort {
		t.Errorf("port = %d, want default %d", port, DefaultGatewayPort)
	}
	if !useTLS {
		t.Error("useTLS = false, want true by default")
	}
}

// TestExposeWhatsAppGatewayRoute_NoGatewayIsSoftNoop verifies the local CLI path
// (no in-process gateway) does not hard-fail on route exposure -- the separate
// `citadel work` gateway self-registers, and VerifyReachable is the real guard.
func TestExposeWhatsAppGatewayRoute_NoGatewayIsSoftNoop(t *testing.T) {
	setProvisionedServiceGateway(nil, 0, false)
	if err := exposeWhatsAppGatewayRoute(8137); err != nil {
		t.Fatalf("exposeWhatsAppGatewayRoute with no gateway = %v, want nil (soft no-op)", err)
	}
}
