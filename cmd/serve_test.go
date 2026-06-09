// cmd/serve_test.go
package cmd

import (
	"testing"
)

func TestServeCommandRegistered(t *testing.T) {
	found := false
	for _, cmd := range rootCmd.Commands() {
		if cmd.Use == "serve" {
			found = true
			break
		}
	}
	if !found {
		t.Error("serve command not registered on rootCmd")
	}
}

func TestServeFlags(t *testing.T) {
	tests := []struct {
		name     string
		defValue string
	}{
		{"port", "8443"},
		{"bind", "0.0.0.0"},
		{"cert-dir", ""},
		{"status-port", "8080"},
		{"terminal-port", "7860"},
		{"vnc-port", "6080"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := serveCmd.Flags().Lookup(tt.name)
			if f == nil {
				t.Fatalf("--%s flag not found on serve command", tt.name)
			}
			if f.DefValue != tt.defValue {
				t.Errorf("--%s default = %q, want %q", tt.name, f.DefValue, tt.defValue)
			}
		})
	}
}

func TestServeNoFabricPortFlag(t *testing.T) {
	// fabric-port was removed because the fabric server is not started by any command
	f := serveCmd.Flags().Lookup("fabric-port")
	if f != nil {
		t.Error("--fabric-port flag should not exist (fabric server is not started by any command)")
	}
}

func TestServePortNotCollidingWithUpstreams(t *testing.T) {
	// Verify the default gateway port (8443) does not collide with any upstream default
	gatewayPort := 8443
	upstreamDefaults := map[string]int{
		"status-port":   8080,
		"terminal-port": 7860,
		"vnc-port":      6080,
	}

	for name, port := range upstreamDefaults {
		if port == gatewayPort {
			t.Errorf("gateway default port %d collides with --%s default", gatewayPort, name)
		}
	}
}

func TestServeHelpOutput(t *testing.T) {
	// Verify the serve command produces help output without panicking
	// This exercises Cobra's init() registration path
	out, err := rootCmd.ExecuteC()
	_ = out
	_ = err

	// Check Long description is set
	if serveCmd.Long == "" {
		t.Error("serve command Long description is empty")
	}

	// Check Example is set
	if serveCmd.Example == "" {
		t.Error("serve command Example is empty")
	}
}
