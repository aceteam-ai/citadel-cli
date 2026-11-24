package cmd

import "testing"

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
		if got := warnGatewayLANBind("--gateway-bind", host); got != want {
			t.Errorf("host %q: warned=%v want %v", host, got, want)
		}
	}
}
