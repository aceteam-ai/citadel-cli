package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	pmx "github.com/aceteam-ai/citadel-cli/internal/proxmox"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	proxmoxURL         string
	proxmoxTokenID     string
	proxmoxTokenSecret string
	proxmoxForgetYes   bool
)

var proxmoxCmd = &cobra.Command{
	Use:   "proxmox",
	Short: "Manage Proxmox VE hypervisor",
	Long:  `Commands for configuring and interacting with a Proxmox VE hypervisor.`,
}

var proxmoxSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Configure Proxmox VE connection",
	Long: `Configure the connection to a Proxmox VE server.

The connection is tested before saving. API token authentication is recommended
over password auth because tokens don't expire and can be scoped to specific
permissions.

To create an API token in Proxmox:
  1. Go to Datacenter > Permissions > API Tokens
  2. Add a new token for your user (e.g., root@pam)
  3. Uncheck "Privilege Separation" for full access
  4. Copy the token ID and secret

Examples:
  # Setup with API token (recommended)
  citadel proxmox setup \
    --url https://192.168.2.4:8006 \
    --token-id "root@pam!citadel" \
    --token-secret "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

  # Just test connection with existing config
  citadel proxmox setup`,
	RunE: runProxmoxSetup,
}

var proxmoxStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show Proxmox VE status and guests",
	Long:  `Display the status of the configured Proxmox VE server, including nodes, VMs, and containers.`,
	RunE:  runProxmoxStatus,
}

var proxmoxForgetCmd = &cobra.Command{
	Use:   "forget",
	Short: "Forget the saved Proxmox connection",
	Long: `Delete the saved Proxmox configuration file (proxmox.json).

The Control Center shows a Proxmox tab whenever a saved config exists, even if
this machine isn't a Proxmox host — the config may point at a remote server.
Forgetting the connection removes that file so the tab no longer appears.

The config file path is printed before deletion. Use --yes to skip the prompt.`,
	RunE: runProxmoxForget,
}

func init() {
	proxmoxSetupCmd.Flags().StringVar(&proxmoxURL, "url", "", "Proxmox VE URL (e.g., https://192.168.2.4:8006)")
	proxmoxSetupCmd.Flags().StringVar(&proxmoxTokenID, "token-id", "", "API token ID (e.g., root@pam!citadel)")
	proxmoxSetupCmd.Flags().StringVar(&proxmoxTokenSecret, "token-secret", "", "API token secret (UUID)")

	proxmoxForgetCmd.Flags().BoolVar(&proxmoxForgetYes, "yes", false, "Skip the confirmation prompt.")

	proxmoxCmd.AddCommand(proxmoxSetupCmd)
	proxmoxCmd.AddCommand(proxmoxStatusCmd)
	proxmoxCmd.AddCommand(proxmoxForgetCmd)
	rootCmd.AddCommand(proxmoxCmd)
}

func runProxmoxSetup(cmd *cobra.Command, args []string) error {
	configDir := platform.ConfigDir()
	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)
	red := color.New(color.FgRed)

	// Load existing config
	existing, _ := pmx.LoadConfig(configDir)

	// Merge flags with existing config
	cfg := &pmx.Config{}
	if existing != nil {
		*cfg = *existing
	}
	if proxmoxURL != "" {
		cfg.BaseURL = proxmoxURL
	}
	if proxmoxTokenID != "" {
		cfg.TokenID = proxmoxTokenID
	}
	if proxmoxTokenSecret != "" {
		cfg.TokenSecret = proxmoxTokenSecret
	}

	// If no URL provided and no existing config, try auto-detect
	if cfg.BaseURL == "" {
		bold.Println("No Proxmox URL configured.")
		fmt.Println("Checking if this host is a Proxmox node...")
		pveInfo, err := platform.DetectProxmox()
		if err == nil && pveInfo.IsInstalled {
			green.Printf("  Proxmox detected: %s (node: %s)\n", pveInfo.Version, pveInfo.NodeName)
			cfg.BaseURL = "https://localhost:8006"
			cfg.NodeName = pveInfo.NodeName
			fmt.Printf("  Using URL: %s\n", cfg.BaseURL)
		} else {
			red.Println("  Not a Proxmox host.")
			fmt.Println("\nUse --url to specify the Proxmox VE server URL:")
			fmt.Println("  citadel proxmox setup --url https://192.168.2.4:8006")
			return fmt.Errorf("no Proxmox URL configured")
		}
	}

	if cfg.TokenID == "" || cfg.TokenSecret == "" {
		fmt.Println("\nNo API token configured. Connection test will use unauthenticated access.")
		fmt.Println("For full access, provide --token-id and --token-secret.")
	}

	// Test connection
	bold.Println("\nTesting connection...")
	client := pmx.NewClient(pmx.ClientConfig{
		BaseURL:     cfg.BaseURL,
		TokenID:     cfg.TokenID,
		TokenSecret: cfg.TokenSecret,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.Ping(ctx); err != nil {
		red.Printf("  Connection failed: %v\n", err)
		return fmt.Errorf("connection test failed: %w", err)
	}
	green.Println("  Connected!")

	// List nodes
	nodes, err := client.ListNodes(ctx)
	if err != nil {
		fmt.Printf("  Warning: could not list nodes: %v\n", err)
	} else {
		fmt.Printf("  Nodes: %d\n", len(nodes))
		for _, n := range nodes {
			status := color.GreenString(n.Status)
			if n.Status != "online" {
				status = color.RedString(n.Status)
			}
			fmt.Printf("    - %s (%s)\n", n.Node, status)
		}

		// Set node name from first online node if not already set
		if cfg.NodeName == "" && len(nodes) > 0 {
			for _, n := range nodes {
				if n.Status == "online" {
					cfg.NodeName = n.Node
					break
				}
			}
			if cfg.NodeName == "" {
				cfg.NodeName = nodes[0].Node
			}
		}
	}

	// Save config
	if err := pmx.SaveConfig(configDir, cfg); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	green.Printf("\nConfiguration saved to %s/proxmox.json\n", configDir)

	return nil
}

func runProxmoxStatus(cmd *cobra.Command, args []string) error {
	configDir := platform.ConfigDir()

	fmt.Printf("Config: %s\n", pmx.ConfigPath(configDir))

	client, err := pmx.ClientFromConfig(configDir)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if client == nil {
		fmt.Println("Proxmox not configured. Run: citadel proxmox setup")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)
	red := color.New(color.FgRed)

	// List nodes
	nodes, err := client.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	bold.Println("Proxmox VE Cluster")
	fmt.Printf("  Nodes: %d\n\n", len(nodes))

	for _, node := range nodes {
		status := green.Sprint(node.Status)
		if node.Status != "online" {
			status = red.Sprint(node.Status)
		}

		cpuPct := node.CPU * 100
		memPct := float64(0)
		if node.MaxMem > 0 {
			memPct = float64(node.Mem) / float64(node.MaxMem) * 100
		}

		bold.Printf("  Node: %s (%s)\n", node.Node, status)
		fmt.Printf("    CPU: %.0f%% (%d cores)  Memory: %.0f%%  Uptime: %s\n",
			cpuPct, node.MaxCPU, memPct, pmxFormatDuration(node.Uptime))

		// List guests
		guests, err := client.ListAllGuests(ctx, node.Node)
		if err != nil {
			fmt.Printf("    Error listing guests: %v\n", err)
			continue
		}

		if len(guests) == 0 {
			fmt.Println("    No VMs or containers")
			continue
		}

		fmt.Printf("    Guests: %d\n", len(guests))
		for _, g := range guests {
			gType := "VM"
			if g.Type == "lxc" {
				gType = "CT"
			}
			gStatus := green.Sprint(g.Status)
			if g.Status != "running" {
				gStatus = red.Sprint(g.Status)
			}

			name := g.Name
			if name == "" {
				name = fmt.Sprintf("%s %d", gType, g.VMID)
			}

			memStr := ""
			if g.MaxMem > 0 {
				memStr = fmt.Sprintf("  Mem: %s", pmxFormatBytesShort(g.MaxMem))
			}

			uptimeStr := ""
			if g.Uptime > 0 {
				uptimeStr = fmt.Sprintf("  Up: %s", pmxFormatDuration(g.Uptime))
			}

			fmt.Printf("      %s %d  %-20s  %s%s%s\n",
				gType, g.VMID, name, gStatus, memStr, uptimeStr)
		}
		fmt.Println()
	}

	return nil
}

func runProxmoxForget(cmd *cobra.Command, args []string) error {
	configDir := platform.ConfigDir()
	path := pmx.ConfigPath(configDir)

	bold := color.New(color.Bold)
	green := color.New(color.FgGreen)

	cfg, _ := pmx.LoadConfig(configDir)
	if cfg == nil || cfg.BaseURL == "" {
		fmt.Printf("No saved Proxmox connection to forget (looked for %s).\n", path)
		return nil
	}

	bold.Println("Forget saved Proxmox connection")
	fmt.Printf("  Base URL: %s\n", cfg.BaseURL)
	fmt.Printf("  Config:   %s\n", path)

	if !proxmoxForgetYes && !proxmoxConfirm(fmt.Sprintf("\nDelete %s?", path)) {
		fmt.Println("Aborted.")
		return nil
	}

	if err := pmx.DeleteConfig(configDir); err != nil {
		return fmt.Errorf("forgetting proxmox connection: %w", err)
	}

	green.Printf("Forgot Proxmox connection. Deleted %s\n", path)
	fmt.Println("The Control Center Proxmox tab will no longer appear (unless this host is itself a Proxmox node).")
	return nil
}

// proxmoxConfirm reads a single yes/no line from stdin and reports whether it
// is affirmative. The default is no.
func proxmoxConfirm(question string) bool {
	fmt.Printf("%s (y/N) ", question)
	reader := bufio.NewReader(os.Stdin)
	resp, _ := reader.ReadString('\n')
	resp = strings.TrimSpace(strings.ToLower(resp))
	return resp == "y" || resp == "yes"
}

func pmxFormatDuration(seconds int64) string {
	d := seconds / 86400
	h := (seconds % 86400) / 3600
	m := (seconds % 3600) / 60

	if d > 0 {
		return fmt.Sprintf("%dd %dh", d, h)
	}
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func pmxFormatBytesShort(b int64) string {
	const (
		kB = 1024
		mB = kB * 1024
		gB = mB * 1024
	)
	switch {
	case b >= gB:
		return fmt.Sprintf("%.1fG", float64(b)/float64(gB))
	case b >= mB:
		return fmt.Sprintf("%.0fM", float64(b)/float64(mB))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
