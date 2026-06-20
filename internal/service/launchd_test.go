//go:build darwin

package service

import (
	"strings"
	"testing"
)

func TestGeneratePlist(t *testing.T) {
	cfg := ServiceConfig{
		ExecPath:    "/usr/local/bin/citadel",
		Args:        []string{"work", "--gateway"},
		Description: "Citadel Node Agent",
		UserMode:    true,
	}

	content, err := GeneratePlist(cfg)
	if err != nil {
		t.Fatalf("GeneratePlist() error: %v", err)
	}

	checks := []struct {
		label    string
		contains string
	}{
		{"label", "<string>ai.aceteam.citadel</string>"},
		{"exec path", "<string>/usr/local/bin/citadel</string>"},
		{"arg work", "<string>work</string>"},
		{"arg gateway", "<string>--gateway</string>"},
		{"run at load", "<key>RunAtLoad</key>"},
		{"keep alive", "<key>KeepAlive</key>"},
		{"env citadel service", "<string>true</string>"},
		{"env key", "<key>CITADEL_SERVICE</key>"},
		{"stdout log", "citadel.log"},
		{"stderr log", "citadel-error.log"},
		{"xml header", "<?xml version=\"1.0\""},
	}

	for _, c := range checks {
		t.Run(c.label, func(t *testing.T) {
			if !strings.Contains(content, c.contains) {
				t.Errorf("plist missing %q:\n%s", c.contains, content)
			}
		})
	}
}

func TestGeneratePlist_NoArgs(t *testing.T) {
	cfg := ServiceConfig{
		ExecPath: "/usr/local/bin/citadel",
		UserMode: true,
	}

	content, err := GeneratePlist(cfg)
	if err != nil {
		t.Fatalf("GeneratePlist() error: %v", err)
	}

	if !strings.Contains(content, "<string>/usr/local/bin/citadel</string>") {
		t.Errorf("plist missing exec path:\n%s", content)
	}

	// Should not contain "work" since no args were provided.
	if strings.Contains(content, "<string>work</string>") {
		t.Error("plist should not contain 'work' when no args provided")
	}
}

func TestPlistPath(t *testing.T) {
	// User mode.
	userPath, err := plistPath(true)
	if err != nil {
		t.Fatalf("plistPath(true) error: %v", err)
	}
	if !strings.Contains(userPath, "Library/LaunchAgents/ai.aceteam.citadel.plist") {
		t.Errorf("unexpected user plist path: %s", userPath)
	}

	// System mode.
	sysPath, err := plistPath(false)
	if err != nil {
		t.Fatalf("plistPath(false) error: %v", err)
	}
	if sysPath != "/Library/LaunchDaemons/ai.aceteam.citadel.plist" {
		t.Errorf("unexpected system plist path: %s", sysPath)
	}
}
