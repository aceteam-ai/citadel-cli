// cmd/vnc_test.go
package cmd

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func TestVNCCommandRegistered(t *testing.T) {
	// Verify vnc command is registered on rootCmd
	found := false
	for _, cmd := range rootCmd.Commands() {
		if cmd.Use == "vnc" {
			found = true
			break
		}
	}
	if !found {
		t.Error("vnc command not registered on rootCmd")
	}
}

func TestVNCSubcommandsRegistered(t *testing.T) {
	expected := map[string]bool{
		"enable":  false,
		"disable": false,
		"status":  false,
	}

	for _, cmd := range vncCmd.Commands() {
		if _, ok := expected[cmd.Use]; ok {
			expected[cmd.Use] = true
		}
	}

	for name, found := range expected {
		if !found {
			t.Errorf("vnc subcommand %q not registered", name)
		}
	}
}

func TestVNCEnableFlags(t *testing.T) {
	// Verify --password flag exists
	pwFlag := vncEnableCmd.Flags().Lookup("password")
	if pwFlag == nil {
		t.Fatal("--password flag not found on vnc enable")
	}
	if pwFlag.DefValue != "" {
		t.Errorf("--password default = %q, want empty string", pwFlag.DefValue)
	}

	// Verify --port flag exists with correct default
	portFlag := vncEnableCmd.Flags().Lookup("port")
	if portFlag == nil {
		t.Fatal("--port flag not found on vnc enable")
	}
	if portFlag.DefValue != "5900" {
		t.Errorf("--port default = %q, want %q", portFlag.DefValue, "5900")
	}
}

func TestVNCEnablePortDefault(t *testing.T) {
	if platform.DefaultVNCPort != 5900 {
		t.Errorf("DefaultVNCPort = %d, want 5900", platform.DefaultVNCPort)
	}
}
