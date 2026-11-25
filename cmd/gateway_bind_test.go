package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestGatewayBindDefaultsAreLoopback(t *testing.T) {
	if !gatewayBindIsLoopback(defaultGatewayBind) {
		t.Fatalf("default gateway bind %q must be loopback", defaultGatewayBind)
	}
	if f := workCmd.Flags().Lookup("gateway-bind"); f == nil || f.DefValue != defaultGatewayBind {
		t.Fatalf("--gateway-bind default must be the loopback default")
	}
	if f := serveCmd.Flags().Lookup("bind"); f == nil || f.DefValue != defaultGatewayBind {
		t.Fatalf("serve --bind default must be the loopback default")
	}
}

func TestWarnGatewayLANBind(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": false, "::1": false, "localhost": false,
		"0.0.0.0": true, "192.168.1.5": true, "": true,
	} {
		var buf bytes.Buffer
		if got := warnGatewayLANBindTo(&buf, "--gateway-bind", host); got != want {
			t.Errorf("host %q: warned=%v want %v", host, got, want)
		}
		if (buf.Len() > 0) != want {
			t.Errorf("host %q: output %q inconsistent with warned=%v", host, buf.String(), want)
		}
		if want && !strings.Contains(buf.String(), "reachable by LAN callers") {
			t.Errorf("host %q: warning must state LAN reachability plainly: %q", host, buf.String())
		}
	}
}

func TestGatewayCmdListenAddr(t *testing.T) {
	if got := gatewayListenAddr("", 8080); got != "127.0.0.1:8080" {
		t.Fatalf("default = %q, want loopback", got)
	}
	if got := gatewayListenAddr("0.0.0.0", 9090); got != "0.0.0.0:9090" {
		t.Fatalf("opt-in = %q", got)
	}
	if f := gatewayCmd.Flags().Lookup("bind"); f == nil || f.DefValue != defaultGatewayBind {
		t.Fatalf("citadel gateway --bind default must be loopback")
	}
}
