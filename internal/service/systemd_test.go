//go:build linux

package service

import (
	"strings"
	"testing"
)

func TestGenerateUnitFile_UserMode(t *testing.T) {
	cfg := ServiceConfig{
		ExecPath:    "/usr/local/bin/citadel",
		Args:        []string{"work", "--gateway"},
		Description: "Citadel Node Agent",
		UserMode:    true,
	}

	content, err := GenerateUnitFile(cfg)
	if err != nil {
		t.Fatalf("GenerateUnitFile() error: %v", err)
	}

	// Verify key fields.
	checks := []struct {
		label    string
		contains string
	}{
		{"description", "Description=Citadel Node Agent"},
		{"exec start", "ExecStart=/usr/local/bin/citadel work --gateway"},
		{"restart", "Restart=on-failure"},
		{"restart sec", "RestartSec=10"},
		// Crash-loop hardening (#443): exponential restart backoff + start-limit cooldown.
		{"restart steps", "RestartSteps=5"},
		{"restart max delay", "RestartMaxDelaySec=300"},
		{"start limit interval", "StartLimitIntervalSec=300"},
		{"start limit burst", "StartLimitBurst=5"},
		{"env var", "Environment=CITADEL_SERVICE=true"},
		{"wanted by", "WantedBy=default.target"},
		{"output", "StandardOutput=journal+console"},
		{"error", "StandardError=journal+console"},
		{"network", "After=network-online.target"},
	}

	for _, c := range checks {
		t.Run(c.label, func(t *testing.T) {
			if !strings.Contains(content, c.contains) {
				t.Errorf("unit file missing %q:\n%s", c.contains, content)
			}
		})
	}

	// User units should NOT have User/Group/ProtectHome directives.
	forbidden := []string{"User=", "Group=", "ProtectHome=", "NoNewPrivileges="}
	for _, f := range forbidden {
		if strings.Contains(content, f) {
			t.Errorf("user unit should not contain %q", f)
		}
	}
}

func TestGenerateUnitFile_SystemMode(t *testing.T) {
	// System mode calls user.Lookup which requires a real user. We can't
	// easily test this without root / mock, but we CAN test the user-mode
	// path comprehensively and verify the system template structure through
	// the generateSystemUnit helper by verifying key template strings.
	//
	// We verify the template indirectly: generateSystemUnit is called by
	// GenerateUnitFile when UserMode=false, and it requires SUDO_USER to
	// resolve the user. In CI this may not be set, so we just verify that
	// the error message is helpful when the lookup fails.
	cfg := ServiceConfig{
		ExecPath:    "/usr/local/bin/citadel",
		Args:        []string{"work"},
		Description: "Test",
		UserMode:    false,
	}

	content, err := GenerateUnitFile(cfg)
	if err != nil {
		// Expected on CI where SUDO_USER points to a non-existent user.
		// Just verify it's a lookup error, not a panic.
		if !strings.Contains(err.Error(), "lookup") && !strings.Contains(err.Error(), "user") {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}

	// If it succeeded (e.g. SUDO_USER was empty -> root), verify structure.
	mustContain := []string{
		"ExecStart=/usr/local/bin/citadel work",
		"Environment=CITADEL_SERVICE=true",
		"WantedBy=multi-user.target",
		"Restart=on-failure",
		"NoNewPrivileges=true",
	}
	for _, s := range mustContain {
		if !strings.Contains(content, s) {
			t.Errorf("system unit missing %q:\n%s", s, content)
		}
	}

	// With ProtectHome=read-only, the tsnet state dir (<home>/citadel-node) MUST
	// be granted write access, else the machine key cannot persist and the node
	// re-registers as a duplicate on every restart (aceteam-ai/citadel-cli#383).
	rwLine := ""
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "ReadWritePaths=") {
			rwLine = line
			break
		}
	}
	if rwLine == "" {
		t.Fatalf("system unit missing ReadWritePaths directive:\n%s", content)
	}
	if !strings.Contains(rwLine, "citadel-node") {
		t.Errorf("ReadWritePaths must include the citadel-node state dir, got %q", rwLine)
	}
	if !strings.Contains(rwLine, ".citadel-cli") {
		t.Errorf("ReadWritePaths must still include the .citadel-cli config dir, got %q", rwLine)
	}
}

func TestGenerateUnitFile_DefaultDescription(t *testing.T) {
	cfg := ServiceConfig{
		ExecPath: "/usr/local/bin/citadel",
		Args:     []string{"work"},
		UserMode: true,
	}

	content, err := GenerateUnitFile(cfg)
	if err != nil {
		t.Fatalf("GenerateUnitFile() error: %v", err)
	}

	if !strings.Contains(content, "Description="+DefaultDescription) {
		t.Errorf("expected default description %q in unit file:\n%s", DefaultDescription, content)
	}
}

func TestGenerateUnitFile_NoArgs(t *testing.T) {
	cfg := ServiceConfig{
		ExecPath: "/usr/local/bin/citadel",
		UserMode: true,
	}

	content, err := GenerateUnitFile(cfg)
	if err != nil {
		t.Fatalf("GenerateUnitFile() error: %v", err)
	}

	// ExecStart should be just the binary, no trailing space.
	if !strings.Contains(content, "ExecStart=/usr/local/bin/citadel\n") {
		t.Errorf("expected ExecStart with no args:\n%s", content)
	}
}

func TestUnitFilePath(t *testing.T) {
	// User mode path.
	userPath, err := unitFilePath(true)
	if err != nil {
		t.Fatalf("unitFilePath(true) error: %v", err)
	}
	if !strings.Contains(userPath, ".config/systemd/user/citadel.service") {
		t.Errorf("unexpected user unit path: %s", userPath)
	}

	// System mode path.
	sysPath, err := unitFilePath(false)
	if err != nil {
		t.Fatalf("unitFilePath(false) error: %v", err)
	}
	if sysPath != "/etc/systemd/system/citadel.service" {
		t.Errorf("unexpected system unit path: %s", sysPath)
	}
}
