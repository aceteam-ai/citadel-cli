package cmd

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/proxmox"
)

func TestInstanceProvisioningEnabledRequiresConstructibleProvider(t *testing.T) {
	tests := []struct {
		name string
		cfg  *proxmox.Config
		want bool
	}{
		{name: "missing config"},
		{name: "missing base URL", cfg: &proxmox.Config{NodeName: "pve1", Provisioning: &proxmox.ProvisioningConfig{Enabled: true, TemplateVMID: 9000}}},
		{name: "disabled", cfg: &proxmox.Config{BaseURL: "https://pve", NodeName: "pve1", Provisioning: &proxmox.ProvisioningConfig{TemplateVMID: 9000}}},
		{name: "non-positive template", cfg: &proxmox.Config{BaseURL: "https://pve", NodeName: "pve1", Provisioning: &proxmox.ProvisioningConfig{Enabled: true}}},
		{name: "missing PVE node", cfg: &proxmox.Config{BaseURL: "https://pve", Provisioning: &proxmox.ProvisioningConfig{Enabled: true, TemplateVMID: 9000}}},
		{name: "connection node fallback", cfg: &proxmox.Config{BaseURL: "https://pve", NodeName: "pve1", Provisioning: &proxmox.ProvisioningConfig{Enabled: true, TemplateVMID: 9000}}, want: true},
		{name: "explicit provisioning node", cfg: &proxmox.Config{BaseURL: "https://pve", Provisioning: &proxmox.ProvisioningConfig{Enabled: true, TemplateVMID: 9000, PVENode: "pve2"}}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.cfg != nil {
				if err := proxmox.SaveConfig(dir, tt.cfg); err != nil {
					t.Fatalf("SaveConfig: %v", err)
				}
			}
			if got := instanceProvisioningEnabled(dir); got != tt.want {
				t.Fatalf("instanceProvisioningEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}
