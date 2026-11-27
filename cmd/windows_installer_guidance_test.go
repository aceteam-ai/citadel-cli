package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Check the printed copy without running an installer, downloading a binary,
// changing PATH or touching Windows ownership and machine-wide settings.
func TestWindowsInstallerStandardUserGuidanceSourceContract(t *testing.T) {
	for _, script := range []string{"install.ps1", "scripts/citadel.ps1"} {
		t.Run(script, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(script)))
			if err != nil {
				t.Fatal(err)
			}
			_, footer, ok := strings.Cut(string(raw), `Write-Host "Next steps:"`)
			if !ok {
				t.Fatal("installer next-steps section is missing")
			}
			standard := footer
			if script == "scripts/citadel.ps1" {
				common, conditional, ok := strings.Cut(footer, "if ($MachineWide) {")
				if !ok {
					t.Fatal("explicit machine-wide guidance branch is missing")
				}
				machine, ordinary, ok := strings.Cut(conditional, "} else {")
				if !ok || !strings.Contains(machine, "citadel up") {
					t.Fatal("machine-wide guidance must remain separate")
				}
				standard = common + ordinary
			}
			for _, required := range []string{
				"citadel local setup", "no admin needed", "Connect this PC to AceTeam",
				"click Connect", "Approve the code", "Keep this PC available to AceTeam",
			} {
				if !strings.Contains(standard, required) {
					t.Errorf("standard-user next steps omit %q", required)
				}
			}
			for _, refused := range []string{
				"Administrator", "elevated", "citadel init", "citadel up", "-MachineWide",
			} {
				if strings.Contains(standard, refused) {
					t.Errorf("standard-user next steps suggest %q", refused)
				}
			}
		})
	}
}
