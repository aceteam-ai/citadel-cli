package controlcenter

import (
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/proxmox"
)

func TestProxmoxConfigLine_SavedConfigShowsPathAndForget(t *testing.T) {
	dir := "/home/user/.citadel-cli"
	line := proxmoxConfigLine(dir, true)

	wantPath := proxmox.ConfigPath(dir)
	if !strings.Contains(line, wantPath) {
		t.Fatalf("config line %q should contain the config path %q", line, wantPath)
	}
	// The forget affordance should be discoverable from this line.
	if !strings.Contains(line, "D") {
		t.Fatalf("config line %q should mention the [D]=forget key", line)
	}
}

func TestProxmoxConfigLine_DetectedHostNoSavedFile(t *testing.T) {
	// A detected local Proxmox host has no saved file: don't print a misleading
	// path or offer to forget a file that isn't there.
	line := proxmoxConfigLine("/home/user/.citadel-cli", false)
	if strings.TrimSpace(line) == "" {
		t.Fatal("config line should not be blank when there is no saved config")
	}
	if !strings.Contains(strings.ToLower(line), "auto-detected") {
		t.Fatalf("no-saved-config line %q should note the auto-detected case", line)
	}
	if strings.Contains(line, "proxmox.json") {
		t.Fatalf("no-saved-config line %q should not print a config file path", line)
	}
}
