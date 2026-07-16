// cmd/instance_ops.go
//
// Live wiring for the INSTANCE_* job family (aceteam#5963): builds the
// proxmox.Provisioner from this node's saved proxmox.json. Provisioning is
// opt-in -- the config must exist AND carry an enabled "provisioning" section
// with a cloud-init template VMID.
package cmd

import (
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/proxmox"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

// newInstanceProviderFactory returns the lazy provider loader the
// InstanceHandler uses. Each job re-reads proxmox.json so config changes are
// picked up without a worker restart.
func newInstanceProviderFactory(configDir string, log func(format string, args ...any)) func() (worker.InstanceProvider, error) {
	return func() (worker.InstanceProvider, error) {
		if configDir == "" {
			return nil, fmt.Errorf("no config directory: this node has no proxmox configuration")
		}
		cfg, err := proxmox.LoadConfig(configDir)
		if err != nil {
			return nil, err
		}
		if cfg == nil || cfg.BaseURL == "" {
			return nil, fmt.Errorf("proxmox is not configured on this node (missing %s)", proxmox.ConfigPath(configDir))
		}
		if cfg.Provisioning == nil || !cfg.Provisioning.Enabled {
			return nil, fmt.Errorf("instance provisioning is not enabled in %s (set provisioning.enabled)", proxmox.ConfigPath(configDir))
		}

		client := proxmox.NewClient(proxmox.ClientConfig{
			BaseURL:     cfg.BaseURL,
			TokenID:     cfg.TokenID,
			TokenSecret: cfg.TokenSecret,
		})

		pcfg := *cfg.Provisioning
		if pcfg.PVENode == "" {
			pcfg.PVENode = cfg.NodeName
		}
		return proxmox.NewProvisioner(client, pcfg, nil, log)
	}
}
