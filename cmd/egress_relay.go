// cmd/egress_relay.go
//
// citadel #787: CLI surface for the on-node SOCKS5 egress relay's config
// (internal/config.EgressRelay). This is one of three surfaces that must
// read/write the SAME persisted egress-relay.yaml (the other two are the
// local_egress_relay_* MCP tools in cmd/mcp_local.go and the
// APPLY_DEVICE_CONFIG handler in internal/jobs/config_handler.go) -- see
// internal/egressrelay's package doc for the feature as a whole.
package cmd

import (
	"fmt"
	"os"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/services"
	"github.com/spf13/cobra"
)

var egressRelayCmd = &cobra.Command{
	Use:   "egress-relay",
	Short: "Manage the on-node SOCKS5 egress relay (citadel #787)",
	Long: `The egress relay lets ANOTHER citadel node on the AceTeam Network tunnel
outbound traffic through THIS node's own network egress -- the server-side
counterpart to 'citadel socks' (which dials OUT through a node from this
machine). It is default OFF and, even when on, only serves a same-org
verified mesh peer: there is no token or passcode fallback.

Changes here take effect on the next 'citadel work' start (the relay listener
is started once at worker startup, not re-evaluated live).`,
}

var egressRelayStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the current egress-relay configuration",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		relay := config.LoadEgressRelay(network.GetNodeConfigDir())
		fmt.Printf("Egress relay:     %s\n", enabledStatusLabel(relay.Enabled))
		fmt.Printf("Allow LAN/mesh:   %s\n", enabledStatusLabel(relay.AllowLAN))
		fmt.Printf("Mesh port:        %d (started only by 'citadel work' when enabled)\n", services.EgressRelayPort)
		if raw := os.Getenv("CITADEL_EGRESS_RELAY"); raw != "" {
			fmt.Printf("\nNote: CITADEL_EGRESS_RELAY=%q is set in this shell's environment and overrides\n", raw)
			fmt.Println("the persisted 'enabled' value for any 'citadel work' started from it.")
		}
		if raw := os.Getenv("CITADEL_EGRESS_ALLOW_LAN"); raw != "" {
			fmt.Printf("Note: CITADEL_EGRESS_ALLOW_LAN=%q is set in this shell's environment and overrides\n", raw)
			fmt.Println("the persisted 'allow_lan' value for any 'citadel work' started from it.")
		}
		return nil
	},
}

var egressRelayEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Enable the egress relay (takes effect on next 'citadel work' start)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return setEgressRelayEnabled(true)
	},
}

var egressRelayDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Disable the egress relay (takes effect on next 'citadel work' start)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return setEgressRelayEnabled(false)
	},
}

var egressRelayAllowLANCmd = &cobra.Command{
	Use:   "allow-lan <on|off>",
	Short: "Allow (or deny) the relay CONNECTing into this node's own LAN/mesh",
	Long: `By default the relay refuses to CONNECT to RFC1918 private addresses,
loopback, link-local, and the 100.64.0.0/10 CGNAT/mesh range -- an authorized
peer can egress to the public internet through this node but cannot pivot
into this node's own LAN or mesh. 'allow-lan on' disables that deny-list.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		switch args[0] {
		case "on":
			return setEgressAllowLAN(true)
		case "off":
			return setEgressAllowLAN(false)
		default:
			return fmt.Errorf("invalid value %q: expected \"on\" or \"off\"", args[0])
		}
	},
}

func enabledStatusLabel(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func setEgressRelayEnabled(enabled bool) error {
	configDir := network.GetNodeConfigDir()
	relay := config.LoadEgressRelay(configDir)
	relay.Enabled = enabled
	if err := config.SaveEgressRelay(configDir, relay); err != nil {
		return fmt.Errorf("failed to save egress relay config: %w", err)
	}
	goodColor.Printf("Egress relay %s.\n", enabledStatusLabel(enabled))
	fmt.Println("Takes effect on the next 'citadel work' start.")
	return nil
}

func setEgressAllowLAN(allow bool) error {
	configDir := network.GetNodeConfigDir()
	relay := config.LoadEgressRelay(configDir)
	relay.AllowLAN = allow
	if err := config.SaveEgressRelay(configDir, relay); err != nil {
		return fmt.Errorf("failed to save egress relay config: %w", err)
	}
	goodColor.Printf("Egress relay LAN/mesh destinations %s.\n", enabledStatusLabel(allow))
	fmt.Println("Takes effect on the next 'citadel work' start.")
	return nil
}

func init() {
	rootCmd.AddCommand(egressRelayCmd)
	egressRelayCmd.AddCommand(egressRelayStatusCmd)
	egressRelayCmd.AddCommand(egressRelayEnableCmd)
	egressRelayCmd.AddCommand(egressRelayDisableCmd)
	egressRelayCmd.AddCommand(egressRelayAllowLANCmd)
}
